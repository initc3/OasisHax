package keymanager

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/libp2p/go-libp2p/core"
	"golang.org/x/exp/slices"

	"github.com/oasisprotocol/curve25519-voi/primitives/x25519"

	beacon "github.com/oasisprotocol/oasis-core/go/beacon/api"
	"github.com/oasisprotocol/oasis-core/go/common"
	cmnBackoff "github.com/oasisprotocol/oasis-core/go/common/backoff"
	"github.com/oasisprotocol/oasis-core/go/common/cbor"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/signature"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
	"github.com/oasisprotocol/oasis-core/go/common/node"
	"github.com/oasisprotocol/oasis-core/go/common/pubsub"
	"github.com/oasisprotocol/oasis-core/go/common/service"
	"github.com/oasisprotocol/oasis-core/go/common/version"
	consensus "github.com/oasisprotocol/oasis-core/go/consensus/api"
	"github.com/oasisprotocol/oasis-core/go/keymanager/api"
	p2p "github.com/oasisprotocol/oasis-core/go/p2p/api"
	"github.com/oasisprotocol/oasis-core/go/p2p/rpc"
	registry "github.com/oasisprotocol/oasis-core/go/registry/api"
	enclaverpc "github.com/oasisprotocol/oasis-core/go/runtime/enclaverpc/api"
	"github.com/oasisprotocol/oasis-core/go/runtime/host"
	"github.com/oasisprotocol/oasis-core/go/runtime/host/protocol"
	"github.com/oasisprotocol/oasis-core/go/runtime/nodes"
	runtimeRegistry "github.com/oasisprotocol/oasis-core/go/runtime/registry"
	scheduler "github.com/oasisprotocol/oasis-core/go/scheduler/api"
	workerCommon "github.com/oasisprotocol/oasis-core/go/worker/common"
	"github.com/oasisprotocol/oasis-core/go/worker/registration"
)

const (
	rpcCallTimeout = 2 * time.Second

	loadEphemeralSecretMaxRetries     = 5
	generateEphemeralSecretMaxRetries = 5
	ephemeralSecretCacheSize          = 20
)

var (
	_ service.BackgroundService = (*Worker)(nil)

	errMalformedResponse = fmt.Errorf("worker/keymanager: malformed response from worker")
)

type runtimeStatus struct {
	version       version.Version
	capabilityTEE *node.CapabilityTEE
}

// Worker is the key manager worker.
//
// It behaves differently from other workers as the key manager has its
// own runtime. It needs to keep track of executor committees for other
// runtimes in order to update the access control lists.
type Worker struct { // nolint: maligned
	sync.RWMutex
	*runtimeRegistry.RuntimeHostNode

	logger *logging.Logger

	ctx       context.Context
	cancelCtx context.CancelFunc
	stopCh    chan struct{}
	quitCh    chan struct{}
	initCh    chan struct{}

	initTicker   *backoff.Ticker
	initTickerCh <-chan time.Time

	runtime runtimeRegistry.Runtime

	clientRuntimes map[common.Namespace]*clientRuntimeWatcher

	accessList          map[core.PeerID]map[common.Namespace]struct{}
	accessListByRuntime map[common.Namespace][]core.PeerID
	privatePeers        map[core.PeerID]struct{}

	commonWorker *workerCommon.Worker
	roleProvider registration.RoleProvider
	backend      api.Backend

	globalStatus   *api.Status
	enclaveStatus  *api.SignedInitResponse
	policy         *api.SignedPolicySGX
	policyChecksum []byte

	numLoadedSecrets    int
	lastLoadedSecret    beacon.EpochTime
	numGeneratedSecrets int
	lastGeneratedSecret beacon.EpochTime

	enabled     bool
	mayGenerate bool
}

func (w *Worker) Name() string {
	return "key manager worker"
}

func (w *Worker) Start() error {
	if !w.enabled {
		w.logger.Info("not starting key manager worker as it is disabled")
		close(w.initCh)

		return nil
	}

	w.logger.Info("starting key manager worker")
	go w.worker()

	return nil
}

func (w *Worker) Stop() {
	w.logger.Info("stopping key manager service")

	if !w.enabled {
		close(w.quitCh)
		return
	}

	// Stop the sub-components.
	w.cancelCtx()
	close(w.stopCh)
}

// Enabled returns if worker is enabled.
func (w *Worker) Enabled() bool {
	return w.enabled
}

func (w *Worker) Quit() <-chan struct{} {
	return w.quitCh
}

func (w *Worker) Cleanup() {
}

// Initialized returns a channel that will be closed when the worker is initialized, ready to
// service requests and registered with the consensus layer.
func (w *Worker) Initialized() <-chan struct{} {
	return w.initCh
}

func (w *Worker) CallEnclave(ctx context.Context, data []byte, kind enclaverpc.Kind) ([]byte, error) {
	switch kind {
	case enclaverpc.KindNoiseSession:
		// Handle access control as only peers on the access list can call this method.
		peerID, ok := rpc.PeerIDFromContext(ctx)
		if !ok {
			return nil, fmt.Errorf("not authorized")
		}

		// Peek into the frame data to extract the method.
		var frame enclaverpc.Frame
		if err := cbor.Unmarshal(data, &frame); err != nil {
			return nil, fmt.Errorf("malformed request")
		}

		// Note that the untrusted plaintext is also checked in the enclave, so if the node lied about
		// what method it's using, we will know and the request will get rejected.
		switch frame.UntrustedPlaintext {
		case "":
			// Anyone can connect.
		case api.RPCMethodGetPublicKey, api.RPCMethodGetPublicEphemeralKey:
			// Anyone can get public keys.
		default:
			if _, privatePeered := w.privatePeers[peerID]; !privatePeered {
				// Defer to access control to check the policy.
				w.RLock()
				_, allowed := w.accessList[peerID]
				w.RUnlock()
				if !allowed {
					return nil, fmt.Errorf("not authorized")
				}
			}
		}
	case enclaverpc.KindInsecureQuery:
		// Insecure queries are always allowed.
	default:
		// Local queries are not allowed.
		return nil, fmt.Errorf("unsupported RPC kind")
	}

	ctx, cancel := context.WithTimeout(ctx, rpcCallTimeout)
	defer cancel()

	// Wait for initialization to complete.
	select {
	case <-w.initCh:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	req := &protocol.Body{
		RuntimeRPCCallRequest: &protocol.RuntimeRPCCallRequest{
			Request: data,
			Kind:    kind,
		},
	}

	// NOTE: Hosted runtime should not be nil as we wait for initialization above.
	rt := w.GetHostedRuntime()
	response, err := rt.Call(ctx, req)
	if err != nil {
		w.logger.Error("failed to dispatch RPC call to runtime",
			"err", err,
		)
		return nil, err
	}

	resp := response.RuntimeRPCCallResponse
	if resp == nil {
		w.logger.Error("malformed response from runtime",
			"response", response,
		)
		return nil, errMalformedResponse
	}

	return resp.Response, nil
}

func (w *Worker) localCallEnclave(method string, args interface{}, rsp interface{}) error {
	req := enclaverpc.Request{
		Method: method,
		Args:   args,
	}
	body := &protocol.Body{
		RuntimeLocalRPCCallRequest: &protocol.RuntimeLocalRPCCallRequest{
			Request: cbor.Marshal(&req),
		},
	}

	rt := w.GetHostedRuntime()
	response, err := rt.Call(w.ctx, body)
	if err != nil {
		w.logger.Error("failed to dispatch local RPC call to runtime",
			"err", err,
		)
		return err
	}

	resp := response.RuntimeLocalRPCCallResponse
	if resp == nil {
		w.logger.Error("malformed response from runtime",
			"response", response,
		)
		return errMalformedResponse
	}

	var msg enclaverpc.Message
	if err = cbor.Unmarshal(resp.Response, &msg); err != nil {
		return fmt.Errorf("malformed message envelope: %w", err)
	}

	if msg.Response == nil {
		return fmt.Errorf("message is not a response: '%s'", hex.EncodeToString(resp.Response))
	}

	switch {
	case msg.Response.Body.Success != nil:
	case msg.Response.Body.Error != nil:
		return fmt.Errorf("rpc failure: '%s'", *msg.Response.Body.Error)
	default:
		return fmt.Errorf("unknown rpc response status: '%s'", hex.EncodeToString(resp.Response))
	}

	if err = cbor.Unmarshal(msg.Response.Body.Success, rsp); err != nil {
		return fmt.Errorf("failed to extract rpc response payload: %w", err)
	}

	return nil
}

func (w *Worker) updateStatus(status *api.Status, runtimeStatus *runtimeStatus) error {
	var initOk bool
	defer func() {
		if !initOk {
			// If initialization failed setup a retry ticker.
			if w.initTicker == nil {
				w.initTicker = backoff.NewTicker(cmnBackoff.NewExponentialBackOff())
				w.initTickerCh = w.initTicker.C
			}
		}
	}()

	// Initialize the key manager.
	var policy []byte
	if status.Policy != nil {
		policy = cbor.Marshal(status.Policy)
	}

	args := api.InitRequest{
		Checksum:    status.Checksum,
		Policy:      policy,
		MayGenerate: w.mayGenerate,
	}

	var signedInitResp api.SignedInitResponse
	if err := w.localCallEnclave(api.RPCMethodInit, args, &signedInitResp); err != nil {
		w.logger.Error("failed to initialize enclave",
			"err", err,
		)
		return fmt.Errorf("worker/keymanager: failed to initialize enclave: %w", err)
	}

	// Validate the signature.
	if tee := runtimeStatus.capabilityTEE; tee != nil {
		var signingKey signature.PublicKey

		switch tee.Hardware {
		case node.TEEHardwareInvalid:
			signingKey = api.InsecureRAK
		case node.TEEHardwareIntelSGX:
			signingKey = tee.RAK
		default:
			return fmt.Errorf("worker/keymanager: unknown TEE hardware: %v", tee.Hardware)
		}

		if err := signedInitResp.Verify(signingKey); err != nil {
			return fmt.Errorf("worker/keymanager: failed to validate initialization response signature: %w", err)
		}
	}

	if !signedInitResp.InitResponse.IsSecure {
		w.logger.Warn("Key manager enclave build is INSECURE")
	}

	w.logger.Info("Key manager initialized",
		"checksum", hex.EncodeToString(signedInitResp.InitResponse.Checksum),
	)
	if w.initTicker != nil {
		w.initTickerCh = nil
		w.initTicker.Stop()
		w.initTicker = nil
	}

	policyUpdateCount.Inc()

	// Register as we are now ready to handle requests.
	initOk = true
	w.roleProvider.SetAvailableWithCallback(func(n *node.Node) error {
		rt := n.AddOrUpdateRuntime(w.runtime.ID(), runtimeStatus.version)
		rt.Version = runtimeStatus.version
		rt.ExtraInfo = cbor.Marshal(signedInitResp)
		rt.Capabilities.TEE = runtimeStatus.capabilityTEE
		return nil
	}, func(context.Context) error {
		w.logger.Info("Key manager registered")

		// Signal that we are initialized.
		select {
		case <-w.initCh:
		default:
			close(w.initCh)
		}
		return nil
	})

	// Cache the key manager enclave status and the currently active policy.
	w.Lock()
	defer w.Unlock()

	w.enclaveStatus = &signedInitResp
	w.policy = status.Policy
	w.policyChecksum = signedInitResp.InitResponse.PolicyChecksum

	return nil
}

func (w *Worker) setStatus(status *api.Status) {
	w.Lock()
	defer w.Unlock()

	w.globalStatus = status
}

func (w *Worker) setLastGeneratedSecretEpoch(epoch beacon.EpochTime) {
	w.Lock()
	defer w.Unlock()

	w.numGeneratedSecrets++
	w.lastGeneratedSecret = epoch
}

func (w *Worker) setLastLoadedSecretEpoch(epoch beacon.EpochTime) {
	w.Lock()
	defer w.Unlock()

	w.numLoadedSecrets++
	w.lastLoadedSecret = epoch
}

func (w *Worker) addClientRuntimeWatcher(n common.Namespace, crw *clientRuntimeWatcher) {
	w.Lock()
	defer w.Unlock()

	w.clientRuntimes[n] = crw
}

func (w *Worker) getClientRuntimeWatcher(n common.Namespace) *clientRuntimeWatcher {
	w.RLock()
	defer w.RUnlock()

	return w.clientRuntimes[n]
}

func (w *Worker) getClientRuntimeWatchers() []*clientRuntimeWatcher {
	w.RLock()
	defer w.RUnlock()

	crws := make([]*clientRuntimeWatcher, 0, len(w.clientRuntimes))
	for _, crw := range w.clientRuntimes {
		crws = append(crws, crw)
	}

	return crws
}

func (w *Worker) startClientRuntimeWatcher(rt *registry.Runtime, status *api.Status) error {
	runtimeID := w.runtime.ID()
	if status == nil || !status.IsInitialized {
		return nil
	}
	if rt.Kind != registry.KindCompute || rt.KeyManager == nil || !rt.KeyManager.Equal(&runtimeID) {
		return nil
	}
	if w.getClientRuntimeWatcher(rt.ID) != nil {
		return nil
	}

	w.logger.Info("seen new runtime using us as a key manager",
		"runtime_id", rt.ID,
	)

	// Check policy document if runtime is allowed to query any of the
	// key manager enclaves.
	var found bool
	switch {
	case !status.IsSecure && status.Policy == nil:
		// Insecure test keymanagers can be without a policy.
		found = true
	case status.Policy != nil:
		for _, enc := range status.Policy.Policy.Enclaves {
			if _, ok := enc.MayQuery[rt.ID]; ok {
				found = true
				break
			}
		}
	}
	if !found {
		w.logger.Warn("runtime not found in keymanager policy, skipping",
			"runtime_id", rt.ID,
			"status", status,
		)
		return nil
	}

	nodes, err := nodes.NewVersionedNodeDescriptorWatcher(w.ctx, w.commonWorker.Consensus)
	if err != nil {
		w.logger.Error("unable to create new client runtime node watcher",
			"err", err,
			"runtime_id", rt.ID,
		)
		return err
	}
	crw := &clientRuntimeWatcher{
		w:         w,
		runtimeID: rt.ID,
		nodes:     nodes,
	}
	crw.epochTransition()
	go crw.worker()

	w.addClientRuntimeWatcher(rt.ID, crw)

	computeRuntimeCount.Inc()

	return nil
}

func (w *Worker) recheckAllRuntimes(status *api.Status) error {
	rts, err := w.commonWorker.Consensus.Registry().GetRuntimes(w.ctx,
		&registry.GetRuntimesQuery{
			Height:           consensus.HeightLatest,
			IncludeSuspended: false,
		},
	)
	if err != nil {
		w.logger.Error("failed querying runtimes",
			"err", err,
		)
		return fmt.Errorf("failed querying runtimes: %w", err)
	}
	for _, rt := range rts {
		if err := w.startClientRuntimeWatcher(rt, status); err != nil {
			w.logger.Error("failed to start runtime watcher",
				"err", err,
			)
			continue
		}
	}

	return nil
}

func (w *Worker) setAccessList(runtimeID common.Namespace, nodes []*node.Node) {
	w.Lock()
	defer w.Unlock()

	// Clear any old nodes from the access list.
	for _, peerID := range w.accessListByRuntime[runtimeID] {
		entry := w.accessList[peerID]
		delete(entry, runtimeID)
		if len(entry) == 0 {
			delete(w.accessList, peerID)
		}
	}

	// Update the access list.
	var peers []core.PeerID
	for _, node := range nodes {
		peerID, err := p2p.PublicKeyToPeerID(node.P2P.ID)
		if err != nil {
			w.logger.Warn("invalid node P2P ID",
				"err", err,
				"node_id", node.ID,
			)
			continue
		}

		entry := w.accessList[peerID]
		if entry == nil {
			entry = make(map[common.Namespace]struct{})
			w.accessList[peerID] = entry
		}

		entry[runtimeID] = struct{}{}
		peers = append(peers, peerID)
	}
	w.accessListByRuntime[runtimeID] = peers

	w.logger.Debug("new client runtime access policy in effect",
		"runtime_id", runtimeID,
		"peers", peers,
	)
}

func (w *Worker) generateEphemeralSecret(runtimeID common.Namespace, epoch beacon.EpochTime, kmStatus *api.Status, runtimeStatus *runtimeStatus) error {
	w.logger.Info("generating ephemeral secret",
		"epoch", epoch,
	)

	// Check if secret has been published. Note that despite this check, the nodes can still publish
	// ephemeral secrets at the same time.
	_, err := w.commonWorker.Consensus.KeyManager().GetEphemeralSecret(w.ctx, &registry.NamespaceEpochQuery{
		Height: consensus.HeightLatest,
		ID:     runtimeID,
		Epoch:  epoch,
	})
	switch err {
	case nil:
		w.logger.Info("skipping secret generation, ephemeral secret already published")
		return nil
	case api.ErrNoSuchEphemeralSecret:
		// Secret hasn't been published.
	default:
		w.logger.Error("failed to fetch ephemeral secret",
			"err", err,
		)
		return fmt.Errorf("failed to fetch ephemeral secret: %w", err)
	}

	// Skip generation if the node is not in the key manager committee.
	id := w.commonWorker.Identity.NodeSigner.Public()
	if !slices.Contains(kmStatus.Nodes, id) {
		w.logger.Info("skipping ephemeral secret generation, node not in the key manager committee")
		return fmt.Errorf("node not in the key manager committee")
	}

	// Generate ephemeral secret.
	args := api.GenerateEphemeralSecretRequest{
		Epoch: epoch,
	}

	var rsp api.GenerateEphemeralSecretResponse
	if err = w.localCallEnclave(api.RPCMethodGenerateEphemeralSecret, args, &rsp); err != nil {
		w.logger.Error("failed to generate ephemeral secret",
			"err", err,
		)
		return fmt.Errorf("failed to generate ephemeral secret: %w", err)
	}

	// Fetch key manager runtime details.
	kmRt, err := w.commonWorker.Consensus.Registry().GetRuntime(w.ctx, &registry.GetRuntimeQuery{
		Height: consensus.HeightLatest,
		ID:     kmStatus.ID,
	})
	if err != nil {
		return err
	}

	// Fetch RAK.
	var rak signature.PublicKey
	switch kmRt.TEEHardware {
	case node.TEEHardwareInvalid:
		rak = api.InsecureRAK
	case node.TEEHardwareIntelSGX:
		if runtimeStatus.capabilityTEE == nil {
			return fmt.Errorf("node doesn't have TEE capability")
		}
		rak = runtimeStatus.capabilityTEE.RAK
	default:
		return fmt.Errorf("TEE hardware mismatch")
	}

	// Fetch REKs of the key manager committee.
	reks := make(map[x25519.PublicKey]struct{})
	for _, id := range kmStatus.Nodes {
		var n *node.Node
		n, err = w.commonWorker.Consensus.Registry().GetNode(w.ctx, &registry.IDQuery{
			Height: consensus.HeightLatest,
			ID:     id,
		})
		switch err {
		case nil:
		case registry.ErrNoSuchNode:
			continue
		default:
			return err
		}

		idx := slices.IndexFunc(n.Runtimes, func(rt *node.Runtime) bool {
			// Skipping version check as key managers are running exactly one
			// version of the runtime.
			return rt.ID == kmStatus.ID
		})
		if idx == -1 {
			continue
		}
		nRt := n.Runtimes[idx]

		var rek x25519.PublicKey
		switch kmRt.TEEHardware {
		case node.TEEHardwareInvalid:
			rek = api.InsecureREK
		case node.TEEHardwareIntelSGX:
			if nRt.Capabilities.TEE == nil || nRt.Capabilities.TEE.REK == nil {
				continue
			}
			rek = *nRt.Capabilities.TEE.REK
		default:
			continue
		}

		reks[rek] = struct{}{}
	}

	// Verify the response.
	if err = rsp.SignedSecret.Verify(epoch, reks, rak); err != nil {
		return fmt.Errorf("failed to validate generate ephemeral secret response signature: %w", err)
	}

	// Publish transaction.
	tx := api.NewPublishEphemeralSecretTx(0, nil, &rsp.SignedSecret)
	if err = consensus.SignAndSubmitTx(w.ctx, w.commonWorker.Consensus, w.commonWorker.Identity.NodeSigner, tx); err != nil {
		return err
	}

	return err
}

func (w *Worker) loadEphemeralSecret(sigSecret *api.SignedEncryptedEphemeralSecret) error {
	w.logger.Info("loading ephemeral secret",
		"epoch", sigSecret.Secret.Epoch,
	)

	args := api.LoadEphemeralSecretRequest{
		SignedSecret: *sigSecret,
	}

	var rsp protocol.Empty
	if err := w.localCallEnclave(api.RPCMethodLoadEphemeralSecret, args, &rsp); err != nil {
		w.logger.Error("failed to load ephemeral secret",
			"err", err,
		)
		return fmt.Errorf("failed to load ephemeral secret: %w", err)
	}

	return nil
}

func (w *Worker) fetchLastEphemeralSecrets(runtimeID common.Namespace) ([]*api.SignedEncryptedEphemeralSecret, error) {
	w.logger.Info("fetching last ephemeral secrets")

	// Get next epoch.
	epoch, err := w.commonWorker.Consensus.Beacon().GetEpoch(w.ctx, consensus.HeightLatest)
	if err != nil {
		w.logger.Error("failed to fetch epoch",
			"err", err,
		)
		return nil, fmt.Errorf("failed to fetch epoch: %w", err)
	}
	epoch++

	// Fetch last few ephemeral secrets.
	N := ephemeralSecretCacheSize
	secrets := make([]*api.SignedEncryptedEphemeralSecret, 0, N)
	for i := 0; i < N && epoch > 0; i, epoch = i+1, epoch-1 {
		secret, err := w.commonWorker.Consensus.KeyManager().GetEphemeralSecret(w.ctx, &registry.NamespaceEpochQuery{
			Height: consensus.HeightLatest,
			ID:     runtimeID,
			Epoch:  epoch,
		})

		switch err {
		case nil:
			secrets = append(secrets, secret)
		case api.ErrNoSuchEphemeralSecret:
			// Secret hasn't been published.
		default:
			w.logger.Error("failed to fetch ephemeral secret",
				"err", err,
			)
			return nil, fmt.Errorf("failed to fetch ephemeral secret: %w", err)
		}
	}

	return secrets, nil
}

// randomBlockHeight returns the height of a random block in the k-th percentile of the given epoch.
func (w *Worker) randomBlockHeight(epoch beacon.EpochTime, percentile int64) (int64, error) {
	// Get height of the first block.
	params, err := w.commonWorker.Consensus.Beacon().ConsensusParameters(w.ctx, consensus.HeightLatest)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch consensus parameters: %w", err)
	}
	first, err := w.commonWorker.Consensus.Beacon().GetEpochBlock(w.ctx, epoch)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch epoch block height: %w", err)
	}

	// Pick a random height from the given percentile.
	interval := params.Interval()
	if percentile < 100 {
		interval = interval * percentile / 100
	}
	if interval <= 0 {
		interval = 1
	}
	height := first + rand.Int63n(interval)

	return height, nil
}

func (w *Worker) worker() { // nolint: gocyclo
	defer close(w.quitCh)

	// Wait for consensus sync.
	w.logger.Info("delaying worker start until after initial synchronization")
	select {
	case <-w.stopCh:
		return
	case <-w.commonWorker.Consensus.Synced():
	}

	// Need to explicitly watch for updates related to the key manager runtime
	// itself.
	knw := newKmNodeWatcher(w)
	go knw.watchNodes()

	// Subscribe to key manager status updates.
	statusCh, statusSub := w.backend.WatchStatuses()
	defer statusSub.Close()

	// Subscribe to key manager ephemeral secret publications.
	entCh, entSub := w.backend.WatchEphemeralSecrets()
	defer entSub.Close()

	// Subscribe to epoch transitions in order to know when we need to refresh
	// the access control policy and choose a random block height for ephemeral
	// secret generation.
	epoCh, epoSub, err := w.commonWorker.Consensus.Beacon().WatchLatestEpoch(w.ctx)
	if err != nil {
		w.logger.Error("failed to watch epochs",
			"err", err,
		)
		return
	}
	defer epoSub.Close()

	// Watch block heights so we can impose a random ephemeral secret
	// generation delay.
	blkCh, blkSub, err := w.commonWorker.Consensus.WatchBlocks(w.ctx)
	if err != nil {
		w.logger.Error("failed to watch blocks",
			"err", err,
		)
		return
	}
	defer blkSub.Close()

	// Subscribe to runtime registrations in order to know which runtimes
	// are using us as a key manager.
	rtCh, rtSub, err := w.commonWorker.Consensus.Registry().WatchRuntimes(w.ctx)
	if err != nil {
		w.logger.Error("failed to watch runtimes",
			"err", err,
		)
		return
	}
	defer rtSub.Close()

	var (
		hrtEventCh           <-chan *host.Event
		currentStatus        *api.Status
		currentRuntimeStatus *runtimeStatus

		epoch beacon.EpochTime

		secret  *api.SignedEncryptedEphemeralSecret
		secrets []*api.SignedEncryptedEphemeralSecret

		loadSecretCh    = make(chan struct{}, 1)
		loadSecretRetry = 0

		genSecretCh         = make(chan struct{}, 1)
		genSecretDoneCh     = make(chan bool, 1)
		genSecretHeight     = int64(math.MaxInt64)
		genSecretInProgress = false
		genSecretRetry      = 0

		runtimeID = w.runtime.ID()
	)
	for {
		select {
		case ev := <-hrtEventCh:
			switch {
			case ev.Started != nil, ev.Updated != nil:
				// Runtime has started successfully.
				currentRuntimeStatus = &runtimeStatus{}
				switch {
				case ev.Started != nil:
					currentRuntimeStatus.version = ev.Started.Version
					currentRuntimeStatus.capabilityTEE = ev.Started.CapabilityTEE
				case ev.Updated != nil:
					currentRuntimeStatus.version = ev.Updated.Version
					currentRuntimeStatus.capabilityTEE = ev.Updated.CapabilityTEE
				default:
					continue
				}

				// Fetch last few ephemeral secrets and send a signal to load them.
				secrets, err = w.fetchLastEphemeralSecrets(runtimeID)
				if err != nil {
					w.logger.Error("failed to fetch last ephemeral secrets",
						"err", err,
					)
				}
				loadSecretRetry = 0
				select {
				case loadSecretCh <- struct{}{}:
				default:
				}

				if currentStatus == nil {
					continue
				}

				// Send a node preregistration, so that other nodes know to update their access
				// control.
				if w.enclaveStatus == nil {
					w.roleProvider.SetAvailable(func(n *node.Node) error {
						rt := n.AddOrUpdateRuntime(w.runtime.ID(), currentRuntimeStatus.version)
						rt.Version = currentRuntimeStatus.version
						rt.ExtraInfo = nil
						rt.Capabilities.TEE = currentRuntimeStatus.capabilityTEE
						return nil
					})
				}

				// Forward status update to key manager runtime.
				if err = w.updateStatus(currentStatus, currentRuntimeStatus); err != nil {
					w.logger.Error("failed to handle status update",
						"err", err,
					)
					continue
				}
			case ev.FailedToStart != nil, ev.Stopped != nil:
				// Worker failed to start or was stopped -- we can no longer service requests.
				currentRuntimeStatus = nil
				w.roleProvider.SetUnavailable()
			default:
				// Unknown event.
				w.logger.Warn("unknown worker event",
					"ev", ev,
				)
			}
		case status := <-statusCh:
			if !status.ID.Equal(&runtimeID) {
				continue
			}

			// Cache the latest status.
			w.setStatus(status)

			// Check if this is the first update and we need to initialize the
			// worker host.
			hrt := w.GetHostedRuntime()
			if hrt == nil {
				// Start key manager runtime.
				w.logger.Info("provisioning key manager runtime")

				var hrtNotifier protocol.Notifier
				hrt, hrtNotifier, err = w.ProvisionHostedRuntime(w.ctx)
				if err != nil {
					w.logger.Error("failed to provision key manager runtime",
						"err", err,
					)
					return
				}

				var sub pubsub.ClosableSubscription
				if hrtEventCh, sub, err = hrt.WatchEvents(w.ctx); err != nil {
					w.logger.Error("failed to subscribe to runtime events",
						"err", err,
					)
					return
				}
				defer sub.Close()

				if err = hrt.Start(); err != nil {
					w.logger.Error("failed to start runtime",
						"err", err,
					)
					return
				}
				defer hrt.Stop()

				if err = hrtNotifier.Start(); err != nil {
					w.logger.Error("failed to start runtime notifier",
						"err", err,
					)
					return
				}
				defer hrtNotifier.Stop()

				// Key managers always need to use the enclave version given to them in the bundle
				// as they need to make sure that replication is possible during upgrades.
				activeVersion := w.runtime.HostVersions()[0] // Init made sure we have exactly one.
				if err = w.SetHostedRuntimeVersion(w.ctx, activeVersion); err != nil {
					w.logger.Error("failed to activate runtime version",
						"err", err,
						"version", activeVersion,
					)
					return
				}
			}

			currentStatus = status
			if currentRuntimeStatus == nil {
				continue
			}

			// Forward status update to key manager runtime.
			if err = w.updateStatus(currentStatus, currentRuntimeStatus); err != nil {
				w.logger.Error("failed to handle status update",
					"err", err,
				)
				continue
			}
			// New runtimes can be allowed with the policy update.
			if err = w.recheckAllRuntimes(currentStatus); err != nil {
				w.logger.Error("failed rechecking runtimes",
					"err", err,
				)
				continue
			}
		case <-w.initTickerCh:
			if currentStatus == nil || currentRuntimeStatus == nil {
				continue
			}
			if err = w.updateStatus(currentStatus, currentRuntimeStatus); err != nil {
				w.logger.Error("failed to handle status update", "err", err)
				continue
			}
			// New runtimes can be allowed with the policy update.
			if err = w.recheckAllRuntimes(currentStatus); err != nil {
				w.logger.Error("failed rechecking runtimes",
					"err", err,
				)
				continue
			}
		case rt := <-rtCh:
			if err = w.startClientRuntimeWatcher(rt, currentStatus); err != nil {
				w.logger.Error("failed to start runtime watcher",
					"err", err,
				)
				continue
			}
		case epoch = <-epoCh:
			// Update per runtime access lists.
			for _, crw := range w.getClientRuntimeWatchers() {
				crw.epochTransition()
			}

			// Choose a random height for ephemeral secret generation. Avoid blocks at the end
			// of the epoch as secret generation, publication and replication takes some time.
			if genSecretHeight, err = w.randomBlockHeight(epoch, 90); err != nil {
				// If randomization fails, the height will be set to zero meaning that the ephemeral
				// secret will be generated immediately without a delay.
				w.logger.Error("failed to select ephemeral secret block height",
					"err", err,
				)
			}
			genSecretRetry = 0

			w.logger.Debug("block height for ephemeral secret generation selected",
				"height", genSecretHeight,
				"epoch", epoch,
			)
		case blk, ok := <-blkCh:
			if !ok {
				w.logger.Error("watch blocks channel closed unexpectedly",
					"err", err,
				)
				return
			}

			// (Re)Generate ephemeral secret once we reach the chosen height.
			if blk.Height >= genSecretHeight {
				select {
				case genSecretCh <- struct{}{}:
				default:
				}
			}

			// (Re)Load ephemeral secrets. When using Tendermint as a backend service the first load
			// will probably fail as the verifier is one block behind.
			if len(secrets) > 0 {
				select {
				case loadSecretCh <- struct{}{}:
				default:
				}
			}
		case secret = <-entCh:
			if secret.Secret.ID != runtimeID {
				continue
			}

			if secret.Secret.Epoch == epoch+1 {
				// Disarm ephemeral secret generation.
				genSecretHeight = math.MaxInt64
			}

			// Add secret to the list and send a signal to load it.
			secrets = append(secrets, secret)
			loadSecretRetry = 0
			select {
			case loadSecretCh <- struct{}{}:
			default:
			}

			w.logger.Debug("ephemeral secret published",
				"epoch", secret.Secret.Epoch,
			)
		case <-genSecretCh:
			if currentStatus == nil || currentRuntimeStatus == nil {
				continue
			}
			if genSecretInProgress || genSecretHeight == math.MaxInt64 {
				continue
			}

			genSecretRetry++
			if genSecretRetry > generateEphemeralSecretMaxRetries {
				// Disarm ephemeral secret generation.
				genSecretHeight = math.MaxInt64
			}

			genSecretInProgress = true

			// Submitting transaction can take time, so don't block the loop.
			go func(epoch beacon.EpochTime, kmStatus *api.Status, rtStatus *runtimeStatus, retry int) {
				err2 := w.generateEphemeralSecret(runtimeID, epoch, kmStatus, rtStatus)
				if err2 != nil {
					w.logger.Error("failed to generate ephemeral secret",
						"err", err2,
						"retry", retry,
					)
					genSecretDoneCh <- false
					return
				}
				genSecretDoneCh <- true
				w.setLastGeneratedSecretEpoch(epoch)
			}(epoch+1, currentStatus, currentRuntimeStatus, genSecretRetry-1)
		case ok := <-genSecretDoneCh:
			// Disarm ephemeral secret generation unless a new height was chosen.
			if ok && genSecretRetry > 0 {
				genSecretHeight = math.MaxInt64
			}
			genSecretInProgress = false
		case <-loadSecretCh:
			var failed []*api.SignedEncryptedEphemeralSecret
			for _, secret := range secrets {
				if err = w.loadEphemeralSecret(secret); err != nil {
					w.logger.Error("failed to load ephemeral secret",
						"err", err,
						"retry", loadSecretRetry,
					)
					failed = append(failed, secret)
					continue
				}
				w.setLastLoadedSecretEpoch(secret.Secret.Epoch)
			}
			secrets = failed

			loadSecretRetry++
			if loadSecretRetry > loadEphemeralSecretMaxRetries {
				// Disarm ephemeral secret loading.
				secrets = nil
			}
		case <-w.stopCh:
			w.logger.Info("termination requested")

			// Wait until ephemeral secret generation running in the background finishes.
			if genSecretInProgress {
				<-genSecretDoneCh
			}

			return
		}
	}
}

type clientRuntimeWatcher struct {
	w         *Worker
	runtimeID common.Namespace
	nodes     nodes.VersionedNodeDescriptorWatcher
}

func (crw *clientRuntimeWatcher) worker() {
	ch, sub, err := crw.nodes.WatchNodeUpdates()
	if err != nil {
		crw.w.logger.Error("failed to subscribe to client runtime node updates",
			"err", err,
			"runtime_id", crw.runtimeID,
		)
		return
	}
	defer sub.Close()

	for {
		select {
		case <-crw.w.ctx.Done():
			return
		case nu := <-ch:
			if nu.Reset {
				// Ignore reset events to avoid clearing the access list before setting a new one.
				// This is safe because a reset event is always followed by a freeze event after the
				// nodes have been set (even if the new set is empty).
				continue
			}
			crw.w.setAccessList(crw.runtimeID, crw.nodes.GetNodes())
		}
	}
}

func (crw *clientRuntimeWatcher) epochTransition() {
	crw.nodes.Reset()

	cms, err := crw.w.commonWorker.Consensus.Scheduler().GetCommittees(crw.w.ctx, &scheduler.GetCommitteesRequest{
		Height:    consensus.HeightLatest,
		RuntimeID: crw.runtimeID,
	})
	if err != nil {
		crw.w.logger.Error("failed to fetch client runtime committee",
			"err", err,
			"runtime_id", crw.runtimeID,
		)
		return
	}

	for _, cm := range cms {
		if cm.Kind != scheduler.KindComputeExecutor {
			continue
		}

		for _, member := range cm.Members {
			_, _ = crw.nodes.WatchNode(crw.w.ctx, member.PublicKey)
		}
	}

	crw.nodes.Freeze(0)

	crw.w.setAccessList(crw.runtimeID, crw.nodes.GetNodes())
}
