// Package api implements the key manager management API and common data types.
package api

import (
	"context"
	"crypto/sha512"
	"fmt"
	"time"

	"github.com/oasisprotocol/curve25519-voi/primitives/x25519"

	beacon "github.com/oasisprotocol/oasis-core/go/beacon/api"
	"github.com/oasisprotocol/oasis-core/go/common"
	"github.com/oasisprotocol/oasis-core/go/common/cbor"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/signature"
	memorySigner "github.com/oasisprotocol/oasis-core/go/common/crypto/signature/signers/memory"
	"github.com/oasisprotocol/oasis-core/go/common/errors"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
	"github.com/oasisprotocol/oasis-core/go/common/node"
	"github.com/oasisprotocol/oasis-core/go/common/pubsub"
	"github.com/oasisprotocol/oasis-core/go/consensus/api/transaction"
	registry "github.com/oasisprotocol/oasis-core/go/registry/api"
)

const (
	// ModuleName is a unique module name for the keymanager module.
	ModuleName = "keymanager"

	// ChecksumSize is the length of checksum in bytes.
	ChecksumSize = 32

	// KeyPairIDSize is the size of a key pair ID in bytes.
	KeyPairIDSize = 32
)

var (
	// ErrInvalidArgument is the error returned on malformed arguments.
	ErrInvalidArgument = errors.New(ModuleName, 1, "keymanager: invalid argument")

	// ErrNoSuchStatus is the error returned when a key manager status does not
	// exist.
	ErrNoSuchStatus = errors.New(ModuleName, 2, "keymanager: no such status")

	// ErrNoSuchEphemeralSecret is the error returned when a key manager ephemeral secret
	// for the given epoch does not exist.
	ErrNoSuchEphemeralSecret = errors.New(ModuleName, 3, "keymanager: no such ephemeral secret")

	// MethodUpdatePolicy is the method name for policy updates.
	MethodUpdatePolicy = transaction.NewMethodName(ModuleName, "UpdatePolicy", SignedPolicySGX{})

	// MethodPublishEphemeralSecret is the method name for publishing ephemeral secret.
	MethodPublishEphemeralSecret = transaction.NewMethodName(ModuleName, "PublishEphemeralSecret", EncryptedEphemeralSecret{})

	// InsecureRAK is the insecure hardcoded key manager public key, used
	// in insecure builds when a RAK is unavailable.
	InsecureRAK signature.PublicKey

	// InsecureREK is the insecure hardcoded key manager public key, used
	// in insecure builds when a REK is unavailable.
	InsecureREK x25519.PublicKey

	// TestSigners contains a list of signers with corresponding test keys, used
	// in insecure builds when a RAK is unavailable.
	TestSigners []signature.Signer

	// Methods is the list of all methods supported by the key manager backend.
	Methods = []transaction.MethodName{
		MethodUpdatePolicy,
		MethodPublishEphemeralSecret,
	}

	// RPCMethodInit is the name of the `init` method.
	RPCMethodInit = "init"

	// RPCMethodGetPublicKey is the name of the `get_public_key` method.
	RPCMethodGetPublicKey = "get_public_key"

	// RPCMethodGetPublicEphemeralKey is the name of the `get_public_ephemeral_key` method.
	RPCMethodGetPublicEphemeralKey = "get_public_ephemeral_key"

	// RPCMethodGenerateEphemeralSecret is the name of the `generate_ephemeral_secret` RPC method.
	RPCMethodGenerateEphemeralSecret = "generate_ephemeral_secret"

	// RPCMethodLoadEphemeralSecret is the name of the `load_ephemeral_secret` RPC method.
	RPCMethodLoadEphemeralSecret = "load_ephemeral_secret"

	// initResponseSignatureContext is the context used to sign key manager init responses.
	initResponseSignatureContext = signature.NewContext("oasis-core/keymanager: init response")
)

const (
	// GasOpUpdatePolicy is the gas operation identifier for policy updates
	// costs.
	GasOpUpdatePolicy transaction.Op = "update_policy"
	// GasOpPublishEphemeralSecret is the gas operation identifier for publishing
	// key manager ephemeral secret.
	GasOpPublishEphemeralSecret transaction.Op = "publish_ephemeral_secret"
)

// XXX: Define reasonable default gas costs.

// DefaultGasCosts are the "default" gas costs for operations.
var DefaultGasCosts = transaction.Costs{
	GasOpUpdatePolicy:           1000,
	GasOpPublishEphemeralSecret: 1000,
}

// KeyPairID is a 256-bit key pair identifier.
type KeyPairID [KeyPairIDSize]byte

// Status is the current key manager status.
type Status struct {
	// ID is the runtime ID of the key manager.
	ID common.Namespace `json:"id"`

	// IsInitialized is true iff the key manager is done initializing.
	IsInitialized bool `json:"is_initialized"`

	// IsSecure is true iff the key manager is secure.
	IsSecure bool `json:"is_secure"`

	// Checksum is the key manager master secret verification checksum.
	Checksum []byte `json:"checksum"`

	// Nodes is the list of currently active key manager node IDs.
	Nodes []signature.PublicKey `json:"nodes"`

	// Policy is the key manager policy.
	Policy *SignedPolicySGX `json:"policy"`

	// RSK is the runtime signing key of the key manager.
	RSK *signature.PublicKey `json:"rsk,omitempty"`
}

// Backend is a key manager management implementation.
type Backend interface {
	// GetStatus returns a key manager status by key manager ID.
	GetStatus(context.Context, *registry.NamespaceQuery) (*Status, error)

	// GetStatuses returns all currently tracked key manager statuses.
	GetStatuses(context.Context, int64) ([]*Status, error)

	// WatchStatuses returns a channel that produces a stream of messages
	// containing the key manager statuses as it changes over time.
	//
	// Upon subscription the current status is sent immediately.
	WatchStatuses() (<-chan *Status, *pubsub.Subscription)

	// StateToGenesis returns the genesis state at specified block height.
	StateToGenesis(context.Context, int64) (*Genesis, error)

	// GetEphemeralSecret returns the key manager ephemeral secret.
	GetEphemeralSecret(context.Context, *registry.NamespaceEpochQuery) (*SignedEncryptedEphemeralSecret, error)

	// WatchEphemeralSecrets returns a channel that produces a stream of ephemeral secrets.
	WatchEphemeralSecrets() (<-chan *SignedEncryptedEphemeralSecret, *pubsub.Subscription)
}

// NewUpdatePolicyTx creates a new policy update transaction.
func NewUpdatePolicyTx(nonce uint64, fee *transaction.Fee, sigPol *SignedPolicySGX) *transaction.Transaction {
	return transaction.NewTransaction(nonce, fee, MethodUpdatePolicy, sigPol)
}

// NewPublishEphemeralSecretTx creates a new publish ephemeral secret transaction.
func NewPublishEphemeralSecretTx(nonce uint64, fee *transaction.Fee, sigEnt *SignedEncryptedEphemeralSecret) *transaction.Transaction {
	return transaction.NewTransaction(nonce, fee, MethodPublishEphemeralSecret, sigEnt)
}

// InitRequest is the initialization RPC request, sent to the key manager
// enclave.
type InitRequest struct {
	Checksum    []byte `json:"checksum"`
	Policy      []byte `json:"policy"`
	MayGenerate bool   `json:"may_generate"`
}

// InitResponse is the initialization RPC response, returned as part of a
// SignedInitResponse from the key manager enclave.
type InitResponse struct {
	IsSecure       bool                 `json:"is_secure"`
	Checksum       []byte               `json:"checksum"`
	PolicyChecksum []byte               `json:"policy_checksum"`
	RSK            *signature.PublicKey `json:"rsk,omitempty"`
}

// SignedInitResponse is the signed initialization RPC response, returned
// from the key manager enclave.
type SignedInitResponse struct {
	InitResponse InitResponse `json:"init_response"`
	Signature    []byte       `json:"signature"`
}

// Verify verifies the signature of the init response using the given key.
func (r *SignedInitResponse) Verify(pk signature.PublicKey) error {
	raw := cbor.Marshal(r.InitResponse)
	if !pk.Verify(initResponseSignatureContext, raw, r.Signature) {
		return fmt.Errorf("keymanager: invalid initialization response signature")
	}
	return nil
}

// SignInitResponse signs the given init response.
func SignInitResponse(signer signature.Signer, response *InitResponse) (*SignedInitResponse, error) {
	sig, err := signer.ContextSign(initResponseSignatureContext, cbor.Marshal(response))
	if err != nil {
		return nil, err
	}
	return &SignedInitResponse{
		InitResponse: *response,
		Signature:    sig,
	}, nil
}

// EphemeralKeyRequest is the ephemeral key RPC request, sent to the key manager
// enclave.
type EphemeralKeyRequest struct {
	Height    *uint64          `json:"height"`
	ID        common.Namespace `json:"runtime_id"`
	KeyPairID KeyPairID        `json:"key_pair_id"`
	Epoch     beacon.EpochTime `json:"epoch"`
}

// SignedPublicKey is the RPC response, returned as part of
// an EphemeralKeyRequest from the key manager enclave.
type SignedPublicKey struct {
	Key        x25519.PublicKey       `json:"key"`
	Checksum   []byte                 `json:"checksum"`
	Signature  signature.RawSignature `json:"signature"`
	Expiration *beacon.EpochTime      `json:"expiration,omitempty"`
}

// GenerateEphemeralSecretRequest is the generate ephemeral secret RPC request,
// sent to the key manager enclave.
type GenerateEphemeralSecretRequest struct {
	Epoch beacon.EpochTime `json:"epoch"`
}

// GenerateEphemeralSecretResponse is the RPC response, returned as part of
// a GenerateEphemeralSecretRequest from the key manager enclave.
type GenerateEphemeralSecretResponse struct {
	SignedSecret SignedEncryptedEphemeralSecret `json:"signed_secret"`
}

// LoadEphemeralSecretRequest is the load ephemeral secret RPC request,
// sent to the key manager enclave.
type LoadEphemeralSecretRequest struct {
	SignedSecret SignedEncryptedEphemeralSecret `json:"signed_secret"`
}

// VerifyExtraInfo verifies and parses the per-node + per-runtime ExtraInfo
// blob for a key manager.
func VerifyExtraInfo(
	logger *logging.Logger,
	nodeID signature.PublicKey,
	rt *registry.Runtime,
	nodeRt *node.Runtime,
	ts time.Time,
	height uint64,
	params *registry.ConsensusParameters,
) (*InitResponse, error) {
	var (
		hw  node.TEEHardware
		rak signature.PublicKey
	)
	if nodeRt.Capabilities.TEE == nil || nodeRt.Capabilities.TEE.Hardware == node.TEEHardwareInvalid {
		hw = node.TEEHardwareInvalid
		rak = InsecureRAK
	} else {
		hw = nodeRt.Capabilities.TEE.Hardware
		rak = nodeRt.Capabilities.TEE.RAK
	}
	if hw != rt.TEEHardware {
		return nil, fmt.Errorf("keymanager: TEEHardware mismatch")
	} else if err := registry.VerifyNodeRuntimeEnclaveIDs(logger, nodeID, nodeRt, rt, params.TEEFeatures, ts, height); err != nil {
		return nil, err
	}
	if nodeRt.ExtraInfo == nil {
		return nil, fmt.Errorf("keymanager: missing ExtraInfo")
	}

	var untrustedSignedInitResponse SignedInitResponse
	if err := cbor.Unmarshal(nodeRt.ExtraInfo, &untrustedSignedInitResponse); err != nil {
		return nil, err
	}
	if err := untrustedSignedInitResponse.Verify(rak); err != nil {
		return nil, err
	}
	return &untrustedSignedInitResponse.InitResponse, nil
}

// Genesis is the key manager management genesis state.
type Genesis struct {
	// Parameters are the key manager consensus parameters.
	Parameters ConsensusParameters `json:"params"`

	Statuses []*Status `json:"statuses,omitempty"`
}

// ConsensusParameters are the key manager consensus parameters.
type ConsensusParameters struct {
	GasCosts transaction.Costs `json:"gas_costs,omitempty"`
}

// ConsensusParameterChanges are allowed key manager consensus parameter changes.
type ConsensusParameterChanges struct {
	// GasCosts are the new gas costs.
	GasCosts transaction.Costs `json:"gas_costs,omitempty"`
}

// Apply applies changes to the given consensus parameters.
func (c *ConsensusParameterChanges) Apply(params *ConsensusParameters) error {
	if c.GasCosts != nil {
		params.GasCosts = c.GasCosts
	}
	return nil
}

// StatusUpdateEvent is the keymanager status update event.
type StatusUpdateEvent struct {
	Statuses []*Status
}

// EventKind returns a string representation of this event's kind.
func (ev *StatusUpdateEvent) EventKind() string {
	return "status"
}

// EphemeralSecretPublishedEvent is the key manager ephemeral secret published event.
type EphemeralSecretPublishedEvent struct {
	Secret *SignedEncryptedEphemeralSecret
}

// EventKind returns a string representation of this event's kind.
func (ev *EphemeralSecretPublishedEvent) EventKind() string {
	return "ephemeral_secret"
}

func init() {
	// Old `INSECURE_SIGNING_KEY_PKCS8`.
	var oldTestKey signature.PublicKey
	_ = oldTestKey.UnmarshalHex("9d41a874b80e39a40c9644e964f0e4f967100c91654bfd7666435fe906af060f")
	signature.RegisterTestPublicKey(oldTestKey)

	// Register all the seed derived SGX key manager test keys.
	for idx, v := range []string{
		"ekiden test key manager RAK seed", // DO NOT REORDER.
		"ekiden key manager test multisig key 0",
		"ekiden key manager test multisig key 1",
		"ekiden key manager test multisig key 2",
	} {
		tmpSigner := memorySigner.NewTestSigner(v)
		TestSigners = append(TestSigners, tmpSigner)

		if idx == 0 {
			InsecureRAK = tmpSigner.Public()
		}
	}

	rek := x25519.PrivateKey(sha512.Sum512_256([]byte("ekiden test key manager REK seed")))
	InsecureREK = *rek.Public()
}
