// Package genesis implements the genesis sub-commands.
package genesis

import (
	"context"
	"encoding/json"
	"errors"
	"io/ioutil"
	"math"
	"math/big"
	"os"
	"time"

	"github.com/spf13/cobra"
	flag "github.com/spf13/pflag"
	"github.com/spf13/viper"

	beacon "github.com/oasislabs/oasis-core/go/beacon/api"
	consensus "github.com/oasislabs/oasis-core/go/common/consensus/genesis"
	"github.com/oasislabs/oasis-core/go/common/crypto/signature"
	"github.com/oasislabs/oasis-core/go/common/entity"
	"github.com/oasislabs/oasis-core/go/common/logging"
	"github.com/oasislabs/oasis-core/go/common/node"
	"github.com/oasislabs/oasis-core/go/common/quantity"
	epochtime "github.com/oasislabs/oasis-core/go/epochtime/api"
	genesis "github.com/oasislabs/oasis-core/go/genesis/api"
	genesisGrpc "github.com/oasislabs/oasis-core/go/grpc/genesis"
	keymanager "github.com/oasislabs/oasis-core/go/keymanager/api"
	cmdCommon "github.com/oasislabs/oasis-core/go/oasis-node/cmd/common"
	"github.com/oasislabs/oasis-core/go/oasis-node/cmd/common/flags"
	cmdGrpc "github.com/oasislabs/oasis-core/go/oasis-node/cmd/common/grpc"
	registry "github.com/oasislabs/oasis-core/go/registry/api"
	roothash "github.com/oasislabs/oasis-core/go/roothash/api"
	"github.com/oasislabs/oasis-core/go/roothash/api/block"
	scheduler "github.com/oasislabs/oasis-core/go/scheduler/api"
	staking "github.com/oasislabs/oasis-core/go/staking/api"
	tendermint "github.com/oasislabs/oasis-core/go/tendermint/api"
)

const (
	cfgEntity             = "entity"
	cfgRuntime            = "runtime"
	cfgNode               = "node"
	cfgRootHash           = "roothash"
	cfgKeyManager         = "keymanager"
	cfgKeyManagerOperator = "keymanager.operator"
	cfgStaking            = "staking"
	cfgBlockHeight        = "height"
	cfgChainID            = "chain.id"
	cfgHaltEpoch          = "halt.epoch"

	// Registry config flags.
	cfgRegistryDebugAllowUnroutableAddresses = "registry.debug.allow_unroutable_addresses"
	cfgRegistryDebugAllowRuntimeRegistration = "registry.debug.allow_runtime_registration"
	cfgRegistryDebugBypassStake              = "registry.debug.bypass_stake" // nolint: gosec

	// Roothash config flags.
	cfgRoundTimeout               = "roothash.round_timeout"
	cfgSchedulerAlgorithm         = "worker.txnscheduler.algorithm"
	cfgSchedulerBatchFlushTimeout = "worker.txnscheduler.flush_timeout"
	cfgSchedulerMaxBatchSize      = "worker.txnscheduler.batching.max_batch_size"
	cfgSchedulerMaxBatchSizeBytes = "worker.txnscheduler.batching.max_batch_size_bytes"

	// Scheduler config flags.
	cfgSchedulerMinValidators            = "scheduler.min_validators"
	cfgSchedulerMaxValidators            = "scheduler.max_validators"
	cfgSchedulerValidatorEntityThreshold = "scheduler.validator_entity_threshold"
	cfgSchedulerDebugBypassStake         = "scheduler.debug.bypass_stake" // nolint: gosec
	cfgSchedulerDebugStaticValidators    = "scheduler.debug.static_validators"

	// Beacon config flags.
	cfgBeaconDebugDeterministic = "beacon.debug.deterministic"

	// EpochTime config flags.
	cfgEpochTimeDebugMockBackend   = "epochtime.debug.mock_backend"
	cfgEpochTimeTendermintInterval = "epochtime.tendermint.interval"

	// Tendermint config flags.
	cfgConsensusTimeoutCommit      = "consensus.tendermint.timeout_commit"
	cfgConsensusSkipTimeoutCommit  = "consensus.tendermint.skip_timeout_commit"
	cfgConsensusEmptyBlockInterval = "consensus.tendermint.empty_block_interval"
	cfgConsensusMaxTxSizeBytes     = "consensus.tendermint.max_tx_size"

	// Consensus backend config flag.
	cfgConsensusBackend = "consensus.backend"

	// Our 'entity' flag overlaps with the common flag 'entity'.
	// We bind it to a separate Viper key to disambiguate at runtime.
	viperEntity = "provision_entity"
)

var (
	dumpGenesisFlags = flag.NewFlagSet("", flag.ContinueOnError)
	initGenesisFlags = flag.NewFlagSet("", flag.ContinueOnError)

	genesisCmd = &cobra.Command{
		Use:   "genesis",
		Short: "genesis block utilities",
	}

	initGenesisCmd = &cobra.Command{
		Use:   "init",
		Short: "initialize the genesis file",
		Run:   doInitGenesis,
	}

	dumpGenesisCmd = &cobra.Command{
		Use:   "dump",
		Short: "dump state into genesis file",
		Run:   doDumpGenesis,
	}

	logger = logging.GetLogger("cmd/genesis")
)

func doInitGenesis(cmd *cobra.Command, args []string) {
	var ok bool
	defer func() {
		if !ok {
			os.Exit(1)
		}
	}()

	if err := cmdCommon.Init(); err != nil {
		cmdCommon.EarlyLogAndExit(err)
	}

	f := flags.GenesisFile()
	if len(f) == 0 {
		logger.Error("failed to determine output location")
		return
	}

	chainID := viper.GetString(cfgChainID)
	if chainID == "" {
		logger.Error("genesis chain id missing")
		return
	}

	// Build the genesis state, if any.
	doc := &genesis.Document{
		ChainID:   chainID,
		Time:      time.Now(),
		HaltEpoch: epochtime.EpochTime(viper.GetUint64(cfgHaltEpoch)),
	}
	entities := viper.GetStringSlice(viperEntity)
	runtimes := viper.GetStringSlice(cfgRuntime)
	nodes := viper.GetStringSlice(cfgNode)
	if err := AppendRegistryState(doc, entities, runtimes, nodes, logger); err != nil {
		logger.Error("failed to parse registry genesis state",
			"err", err,
		)
		return
	}

	rh := viper.GetStringSlice(cfgRootHash)
	if err := AppendRootHashState(doc, rh, logger); err != nil {
		logger.Error("failed to parse roothash genesis state",
			"err", err,
		)
		return
	}

	keymanager := viper.GetStringSlice(cfgKeyManager)
	if err := AppendKeyManagerState(doc, keymanager, logger); err != nil {
		logger.Error("failed to parse key manager genesis state",
			"err", err,
		)
		return
	}

	staking := viper.GetString(cfgStaking)
	if err := AppendStakingState(doc, staking, logger); err != nil {
		logger.Error("failed to parse staking genesis state",
			"err", err,
		)
		return
	}

	doc.Scheduler = scheduler.Genesis{
		Parameters: scheduler.ConsensusParameters{
			MinValidators:            viper.GetInt(cfgSchedulerMinValidators),
			MaxValidators:            viper.GetInt(cfgSchedulerMaxValidators),
			ValidatorEntityThreshold: viper.GetInt(cfgSchedulerValidatorEntityThreshold),
			DebugBypassStake:         viper.GetBool(cfgSchedulerDebugBypassStake),
			DebugStaticValidators:    viper.GetBool(cfgSchedulerDebugStaticValidators),
		},
	}

	doc.Beacon = beacon.Genesis{
		Parameters: beacon.ConsensusParameters{
			DebugDeterministic: viper.GetBool(cfgBeaconDebugDeterministic),
		},
	}

	doc.EpochTime = epochtime.Genesis{
		Parameters: epochtime.ConsensusParameters{
			DebugMockBackend: viper.GetBool(cfgEpochTimeDebugMockBackend),
			Interval:         viper.GetInt64(cfgEpochTimeTendermintInterval),
		},
	}

	doc.Consensus = consensus.Genesis{
		Backend:            viper.GetString(cfgConsensusBackend),
		TimeoutCommit:      viper.GetDuration(cfgConsensusTimeoutCommit),
		SkipTimeoutCommit:  viper.GetBool(cfgConsensusSkipTimeoutCommit),
		EmptyBlockInterval: viper.GetDuration(cfgConsensusEmptyBlockInterval),
		MaxTxSize:          viper.GetSizeInBytes(cfgConsensusMaxTxSizeBytes),
	}

	// TODO: Ensure consistency/sanity.

	b, _ := json.Marshal(doc)
	if err := ioutil.WriteFile(f, b, 0600); err != nil {
		logger.Error("failed to save generated genesis document",
			"err", err,
		)
		return
	}

	ok = true
}

// AppendRegistryState appends the registry genesis state given a vector
// of entity registrations and runtime registrations.
func AppendRegistryState(doc *genesis.Document, entities, runtimes, nodes []string, l *logging.Logger) error {
	regSt := registry.Genesis{
		Parameters: registry.ConsensusParameters{
			DebugAllowUnroutableAddresses: viper.GetBool(cfgRegistryDebugAllowUnroutableAddresses),
			DebugAllowRuntimeRegistration: viper.GetBool(cfgRegistryDebugAllowRuntimeRegistration),
			DebugBypassStake:              viper.GetBool(cfgRegistryDebugBypassStake),
		},
		Entities: make([]*entity.SignedEntity, 0, len(entities)),
		Runtimes: make([]*registry.SignedRuntime, 0, len(runtimes)),
		Nodes:    make([]*node.SignedNode, 0, len(nodes)),
	}

	entMap := make(map[signature.MapKey]bool)
	appendToEntities := func(signedEntity *entity.SignedEntity, ent *entity.Entity) error {
		idKey := ent.ID.ToMapKey()
		if entMap[idKey] {
			return errors.New("genesis: duplicate entity registration")
		}
		entMap[idKey] = true

		regSt.Entities = append(regSt.Entities, signedEntity)

		return nil
	}

	loadSignedEntity := func(fn string) (*entity.SignedEntity, *entity.Entity, error) {
		b, err := ioutil.ReadFile(fn)
		if err != nil {
			return nil, nil, err
		}

		var signedEntity entity.SignedEntity
		if err = json.Unmarshal(b, &signedEntity); err != nil {
			return nil, nil, err
		}

		var ent entity.Entity
		if err := signedEntity.Open(registry.RegisterGenesisEntitySignatureContext, &ent); err != nil {
			return nil, nil, err
		}

		return &signedEntity, &ent, nil
	}

	for _, v := range entities {
		signedEntity, ent, err := loadSignedEntity(v)
		if err != nil {
			l.Error("failed to load genesis entity",
				"err", err,
				"filename", v,
			)
			return err
		}

		if err = appendToEntities(signedEntity, ent); err != nil {
			l.Error("failed to process genesis entity",
				"err", err,
				"filename", v,
			)
		}
	}
	if flags.DebugTestEntity() {
		l.Warn("registering debug test entity")

		ent, signer, err := entity.TestEntity()
		if err != nil {
			l.Error("failed to retrive test entity",
				"err", err,
			)
			return err
		}

		signedEntity, err := entity.SignEntity(signer, registry.RegisterGenesisEntitySignatureContext, ent)
		if err != nil {
			l.Error("failed to sign test entity",
				"err", err,
			)
			return err
		}

		if err = appendToEntities(signedEntity, ent); err != nil {
			l.Error("failed to process test entity",
				"err", err,
			)
			return err
		}

		regSt.Parameters.KeyManagerOperator = ent.ID
	}

	if s := viper.GetString(cfgKeyManagerOperator); s != "" {
		_, ent, err := loadSignedEntity(s)
		if err != nil {
			l.Error("failed to load key manager operator entity",
				"err", err,
				"filename", s,
			)
			return err
		}

		if !entMap[ent.ID.ToMapKey()] {
			l.Error("key manager operator is not a genesis entity",
				"id", ent.ID,
			)
			return registry.ErrNoSuchEntity
		}

		regSt.Parameters.KeyManagerOperator = ent.ID
	}

	for _, v := range runtimes {
		b, err := ioutil.ReadFile(v)
		if err != nil {
			l.Error("failed to load genesis runtime registration",
				"err", err,
				"filename", v,
			)
			return err
		}

		var rt registry.SignedRuntime
		if err = json.Unmarshal(b, &rt); err != nil {
			l.Error("failed to parse genesis runtime registration",
				"err", err,
				"filename", v,
			)
			return err
		}

		regSt.Runtimes = append(regSt.Runtimes, &rt)
	}

	for _, v := range nodes {
		b, err := ioutil.ReadFile(v)
		if err != nil {
			l.Error("failed to load genesis node registration",
				"err", err,
				"filename", v,
			)
			return err
		}

		var n node.SignedNode
		if err = json.Unmarshal(b, &n); err != nil {
			l.Error("failed to parse genesis node registration",
				"err", err,
				"filename", v,
			)
			return err
		}

		regSt.Nodes = append(regSt.Nodes, &n)
	}

	doc.Registry = regSt

	return nil
}

// AppendRootHashState appends the roothash genesis state given a vector
// of exported roothash blocks.
func AppendRootHashState(doc *genesis.Document, exports []string, l *logging.Logger) error {
	rootSt := roothash.Genesis{
		Parameters: roothash.ConsensusParameters{
			RoundTimeout: viper.GetDuration(cfgRoundTimeout),
			TransactionScheduler: roothash.TransactionSchedulerParameters{
				Algorithm:         viper.GetString(cfgSchedulerAlgorithm),
				BatchFlushTimeout: viper.GetDuration(cfgSchedulerBatchFlushTimeout),
				MaxBatchSize:      viper.GetUint64(cfgSchedulerMaxBatchSize),
				MaxBatchSizeBytes: uint64(viper.GetSizeInBytes(cfgSchedulerMaxBatchSizeBytes)),
			},
		},
		Blocks: make(map[signature.MapKey]*block.Block),
	}

	for _, v := range exports {
		b, err := ioutil.ReadFile(v)
		if err != nil {
			l.Error("failed to load genesis roothash blocks",
				"err", err,
				"filename", v,
			)
			return err
		}

		var blocks []*block.Block
		if err = json.Unmarshal(b, &blocks); err != nil {
			l.Error("failed to parse genesis roothash blocks",
				"err", err,
				"filename", v,
			)
			return err
		}

		for _, blk := range blocks {
			var key signature.MapKey
			copy(key[:], blk.Header.Namespace[:])
			if _, ok := rootSt.Blocks[key]; ok {
				l.Error("duplicate genesis roothash block",
					"runtime_id", blk.Header.Namespace,
					"block", blk,
				)
				return errors.New("duplicate genesis roothash block")
			}
			rootSt.Blocks[key] = blk
		}
	}

	doc.RootHash = rootSt

	return nil
}

// AppendKeyManagerState appends the key manager genesis state given a vector of
// key manager statuses.
func AppendKeyManagerState(doc *genesis.Document, statuses []string, l *logging.Logger) error {
	var kmSt keymanager.Genesis

	for _, v := range statuses {
		b, err := ioutil.ReadFile(v)
		if err != nil {
			l.Error("failed to load genesis key manager status",
				"err", err,
				"filename", v,
			)
			return err
		}

		var status keymanager.Status
		if err = json.Unmarshal(b, &status); err != nil {
			l.Error("failed to parse genesis key manager status",
				"err", err,
				"filename", v,
			)
			return err
		}

		kmSt.Statuses = append(kmSt.Statuses, &status)
	}

	doc.KeyManager = kmSt

	return nil
}

// AppendStakingState appends the staking genesis state given a state file name.
func AppendStakingState(doc *genesis.Document, state string, l *logging.Logger) error {
	stakingSt := staking.Genesis{
		Ledger: make(map[signature.MapKey]*staking.Account),
	}

	if state != "" {
		b, err := ioutil.ReadFile(state)
		if err != nil {
			l.Error("failed to load genesis staking status",
				"err", err,
				"filename", state,
			)
			return err
		}

		if err = json.Unmarshal(b, &stakingSt); err != nil {
			l.Error("failed to parse genesis staking status",
				"err", err,
				"filename", state,
			)
			return err
		}
	}
	if flags.DebugTestEntity() {
		l.Warn("granting stake to the debug test entity")

		ent, _, err := entity.TestEntity()
		if err != nil {
			l.Error("failed to retrieve test entity",
				"err", err,
			)
			return err
		}

		// Ok then, we hold the world ransom for One Hundred Billion Dollars.
		var q quantity.Quantity
		if err = q.FromBigInt(big.NewInt(100000000000)); err != nil {
			l.Error("failed to allocate test stake",
				"err", err,
			)
			return err
		}

		stakingSt.Ledger[ent.ID.ToMapKey()] = &staking.Account{
			General: staking.GeneralAccount{
				Balance: q,
				Nonce:   0,
			},
			Escrow: staking.EscrowAccount{
				Active: staking.SharePool{
					Balance: q,
				},
			},
		}

		// Inflate the TotalSupply to account for the account's general and
		// escrow balances.
		_ = stakingSt.TotalSupply.Add(&q)
		_ = stakingSt.TotalSupply.Add(&q)
	}

	doc.Staking = stakingSt

	return nil
}

func doDumpGenesis(cmd *cobra.Command, args []string) {
	ctx := context.Background()

	if err := cmdCommon.Init(); err != nil {
		cmdCommon.EarlyLogAndExit(err)
	}

	conn, err := cmdGrpc.NewClient(cmd)
	if err != nil {
		logger.Error("failed to establish connection with node",
			"err", err,
		)
		os.Exit(1)
	}
	defer conn.Close()

	client := genesisGrpc.NewGenesisClient(conn)

	req := &genesisGrpc.GenesisRequest{
		Height: viper.GetInt64(cfgBlockHeight),
	}
	result, err := client.ToGenesis(ctx, req)
	if err != nil {
		logger.Error("failed to generate genesis document",
			"err", err,
		)
		os.Exit(1)
	}

	w, shouldClose, err := cmdCommon.GetOutputWriter(cmd, flags.CfgGenesisFile)
	if err != nil {
		logger.Error("failed to get writer for genesis file",
			"err", err,
		)
		os.Exit(1)
	}
	if shouldClose {
		defer w.Close()
	}

	if _, err = w.Write(result.Json); err != nil {
		logger.Error("failed to write genesis file",
			"err", err,
		)
		os.Exit(1)
	}
}

// Register registers the genesis sub-command and all of it's children.
func Register(parentCmd *cobra.Command) {
	initGenesisCmd.Flags().AddFlagSet(initGenesisFlags)
	dumpGenesisCmd.Flags().AddFlagSet(dumpGenesisFlags)
	dumpGenesisCmd.PersistentFlags().AddFlagSet(cmdGrpc.ClientFlags)

	for _, v := range []*cobra.Command{
		initGenesisCmd,
		dumpGenesisCmd,
	} {
		genesisCmd.AddCommand(v)
	}

	parentCmd.AddCommand(genesisCmd)
}

func init() {
	dumpGenesisFlags.Int64(cfgBlockHeight, 0, "block height at which to dump state")
	_ = viper.BindPFlags(dumpGenesisFlags)
	dumpGenesisFlags.AddFlagSet(flags.GenesisFileFlags)

	initGenesisFlags.StringSlice(cfgRuntime, nil, "path to runtime registration file")
	initGenesisFlags.StringSlice(cfgNode, nil, "path to node registration file")
	initGenesisFlags.StringSlice(cfgRootHash, nil, "path to roothash genesis blocks file")
	initGenesisFlags.String(cfgStaking, "", "path to staking genesis file")
	initGenesisFlags.StringSlice(cfgKeyManager, nil, "path to key manager genesis status file")
	initGenesisFlags.String(cfgKeyManagerOperator, "", "path to key manager operator entity registration file")
	initGenesisFlags.String(cfgChainID, "", "genesis chain id")
	initGenesisFlags.Uint64(cfgHaltEpoch, math.MaxUint64, "genesis halt epoch height")

	// Registry config flags.
	initGenesisFlags.Bool(cfgRegistryDebugAllowUnroutableAddresses, false, "allow unroutable addreses (UNSAFE)")
	initGenesisFlags.Bool(cfgRegistryDebugAllowRuntimeRegistration, false, "enable non-genesis runtime registration (UNSAFE)")
	initGenesisFlags.Bool(cfgRegistryDebugBypassStake, false, "bypass all stake checks and operations (UNSAFE)")

	// Roothash config flags.
	initGenesisFlags.Duration(cfgRoundTimeout, 10*time.Second, "Root hash round timeout")
	initGenesisFlags.String(cfgSchedulerAlgorithm, "batching", "Transaction scheduling algorithm")
	initGenesisFlags.Duration(cfgSchedulerBatchFlushTimeout, 1*time.Second, "Maximum amount of time to wait for a scheduled batch")
	initGenesisFlags.Uint64(cfgSchedulerMaxBatchSize, 1000, "Maximum size of a batch of runtime requests")
	initGenesisFlags.String(cfgSchedulerMaxBatchSizeBytes, "16mb", "Maximum size (in bytes) of a batch of runtime requests")

	// Scheduler config flags.
	initGenesisFlags.Int(cfgSchedulerMinValidators, 1, "minumum number of validators")
	initGenesisFlags.Int(cfgSchedulerMaxValidators, 100, "maximum number of validators")
	initGenesisFlags.Int(cfgSchedulerValidatorEntityThreshold, 100, "validator entity threshold")
	initGenesisFlags.Bool(cfgSchedulerDebugBypassStake, false, "bypass all stake checks and operations (UNSAFE)")
	initGenesisFlags.Bool(cfgSchedulerDebugStaticValidators, false, "bypass all validator elections (UNSAFE)")

	// Beacon config flags.
	initGenesisFlags.Bool(cfgBeaconDebugDeterministic, false, "enable deterministic beacon output (UNSAFE)")

	// EpochTime config flags.
	initGenesisFlags.Bool(cfgEpochTimeDebugMockBackend, false, "use debug mock Epoch time backend")
	initGenesisFlags.Int64(cfgEpochTimeTendermintInterval, 86400, "Epoch interval (in blocks)")

	// Tendermint config flags.
	initGenesisFlags.Duration(cfgConsensusTimeoutCommit, 1*time.Second, "tendermint commit timeout")
	initGenesisFlags.Bool(cfgConsensusSkipTimeoutCommit, false, "skip tendermint commit timeout")
	initGenesisFlags.Duration(cfgConsensusEmptyBlockInterval, 0*time.Second, "tendermint empty block interval")
	initGenesisFlags.String(cfgConsensusMaxTxSizeBytes, "32kb", "tendermint maximum transaction size (in bytes)")

	// Consensus backend flag.
	initGenesisFlags.String(cfgConsensusBackend, tendermint.BackendName, "consensus backend")

	_ = viper.BindPFlags(initGenesisFlags)
	initGenesisFlags.StringSlice(cfgEntity, nil, "path to entity registration file")
	_ = viper.BindPFlag(viperEntity, initGenesisFlags.Lookup(cfgEntity))
	initGenesisFlags.AddFlagSet(flags.DebugTestEntityFlags)
	initGenesisFlags.AddFlagSet(flags.GenesisFileFlags)
}