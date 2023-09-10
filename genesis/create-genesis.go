package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/common/systemcontract"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/gorilla/mux"
	"io/fs"
	"io/ioutil"
	"math/big"
	"net/http"
	"os"
	"reflect"
	"strings"
	"unicode"
	"unsafe"

	_ "github.com/ethereum/go-ethereum/eth/tracers/native"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
)

type artifactData struct {
	Bytecode         string `json:"bytecode"`
	DeployedBytecode string `json:"deployedBytecode"`
}

type dummyChainContext struct {
}

func (d *dummyChainContext) Engine() consensus.Engine {
	return nil
}

func (d *dummyChainContext) GetHeader(common.Hash, uint64) *types.Header {
	return nil
}

func createExtraData(validators []common.Address) []byte {
	extra := make([]byte, 32+20*len(validators)+65)
	for i, v := range validators {
		copy(extra[32+20*i:], v.Bytes())
	}
	return extra
}

func readStateObjectsFromState(f *state.StateDB) map[common.Address]*state.StateObject {
	var result map[common.Address]*state.StateObject
	rs := reflect.ValueOf(*f)
	rf := rs.FieldByName("stateObjects")
	rs2 := reflect.New(rs.Type()).Elem()
	rs2.Set(rs)
	rf = rs2.FieldByName("stateObjects")
	rf = reflect.NewAt(rf.Type(), unsafe.Pointer(rf.UnsafeAddr())).Elem()
	ri := reflect.ValueOf(&result).Elem()
	ri.Set(rf)
	return result
}

func readDirtyStorageFromState(f *state.StateObject) state.Storage {
	var result map[common.Hash]common.Hash
	rs := reflect.ValueOf(*f)
	rf := rs.FieldByName("dirtyStorage")
	rs2 := reflect.New(rs.Type()).Elem()
	rs2.Set(rs)
	rf = rs2.FieldByName("dirtyStorage")
	rf = reflect.NewAt(rf.Type(), unsafe.Pointer(rf.UnsafeAddr())).Elem()
	ri := reflect.ValueOf(&result).Elem()
	ri.Set(rf)
	return result
}

var stakingAddress = common.HexToAddress("0x0000000000000000000000000000000000001000")
var slashingIndicatorAddress = common.HexToAddress("0x0000000000000000000000000000000000001001")
var systemRewardAddress = common.HexToAddress("0x0000000000000000000000000000000000001002")
var stakingPoolAddress = common.HexToAddress("0x0000000000000000000000000000000000007001")
var governanceAddress = common.HexToAddress("0x0000000000000000000000000000000000007002")
var chainConfigAddress = common.HexToAddress("0x0000000000000000000000000000000000007003")
var runtimeUpgradeAddress = common.HexToAddress("0x0000000000000000000000000000000000007004")
var deployerProxyAddress = common.HexToAddress("0x0000000000000000000000000000000000007005")
var intermediarySystemAddress = common.HexToAddress("0xfffffffffffffffffffffffffffffffffffffffe")

//go:embed build/contracts/RuntimeProxy.json
var runtimeProxyArtifact []byte

//go:embed build/contracts/Staking.json
var stakingRawArtifact []byte

//go:embed build/contracts/StakingPool.json
var stakingPoolRawArtifact []byte

//go:embed build/contracts/ChainConfig.json
var chainConfigRawArtifact []byte

//go:embed build/contracts/SlashingIndicator.json
var slashingIndicatorRawArtifact []byte

//go:embed build/contracts/SystemReward.json
var systemRewardRawArtifact []byte

//go:embed build/contracts/Governance.json
var governanceRawArtifact []byte

//go:embed build/contracts/RuntimeUpgrade.json
var runtimeUpgradeRawArtifact []byte

//go:embed build/contracts/DeployerProxy.json
var deployerProxyRawArtifact []byte

func newArguments(typeNames ...string) abi.Arguments {
	var args abi.Arguments
	for i, tn := range typeNames {
		abiType, err := abi.NewType(tn, tn, nil)
		if err != nil {
			panic(err)
		}
		args = append(args, abi.Argument{Name: fmt.Sprintf("%d", i), Type: abiType})
	}
	return args
}

type consensusParams struct {
	ActiveValidatorsLength   uint32                `json:"activeValidatorsLength"`
	EpochBlockInterval       uint32                `json:"epochBlockInterval"`
	MisdemeanorThreshold     uint32                `json:"misdemeanorThreshold"`
	FelonyThreshold          uint32                `json:"felonyThreshold"`
	ValidatorJailEpochLength uint32                `json:"validatorJailEpochLength"`
	UndelegatePeriod         uint32                `json:"undelegatePeriod"`
	MinValidatorStakeAmount  *math.HexOrDecimal256 `json:"minValidatorStakeAmount"`
	MinStakingAmount         *math.HexOrDecimal256 `json:"minStakingAmount"`
}

type genesisConfig struct {
	ChainId  int64 `json:"chainId"`
	Features struct {
		RuntimeUpgradeBlock *math.HexOrDecimal256 `json:"runtimeUpgradeBlock"`
	} `json:"features"`
	Deployers       []common.Address          `json:"deployers"`
	Validators      []common.Address          `json:"validators"`
	Owners          []common.Address          `json:"owners"`
	SystemTreasury  map[common.Address]uint16 `json:"systemTreasury"`
	ConsensusParams consensusParams           `json:"consensusParams"`
	VotingPeriod    int64                     `json:"votingPeriod"`
	Faucet          map[common.Address]string `json:"faucet"`
	CommissionRate  int64                     `json:"commissionRate"`
	InitialStakes   map[common.Address]string `json:"initialStakes"`
}

func traceCallError(deployedBytecode []byte) {
	for _, c := range deployedBytecode[64:] {
		if c >= 32 && c <= unicode.MaxASCII {
			print(string(c))
		}
	}
	println()
}

func byteCodeFromArtifact(rawArtifact []byte) []byte {
	artifact := &artifactData{}
	if err := json.Unmarshal(rawArtifact, artifact); err != nil {
		panic(err)
	}
	return hexutil.MustDecode(artifact.Bytecode)
}

func mustNewType(t string) abi.Type {
	typ, _ := abi.NewType(t, t, nil)
	return typ
}

func createInitializer(typeNames []string, params []interface{}) []byte {
	initializerArgs, err := newArguments(typeNames...).Pack(params...)
	if err != nil {
		panic(err)
	}
	initializerSig := crypto.Keccak256([]byte(fmt.Sprintf("initialize(%s)", strings.Join(typeNames, ","))))[:4]
	return append(initializerSig, initializerArgs...)
}

func createSimpleBytecode(rawArtifact []byte) []byte {
	constructorArgs, err := newArguments(
		"address", "address", "address", "address", "address", "address", "address", "address").Pack(
		stakingAddress, slashingIndicatorAddress, systemRewardAddress, stakingPoolAddress, governanceAddress, chainConfigAddress, runtimeUpgradeAddress, deployerProxyAddress)
	if err != nil {
		panic(err)
	}
	return append(byteCodeFromArtifact(rawArtifact), constructorArgs...)
}

func createProxyBytecodeWithConstructor(rawArtifact []byte, initTypes []string, initArgs []interface{}) []byte {
	constructorArgs, err := newArguments(
		"address", "address", "address", "address", "address", "address", "address", "address").Pack(
		stakingAddress, slashingIndicatorAddress, systemRewardAddress, stakingPoolAddress, governanceAddress, chainConfigAddress, runtimeUpgradeAddress, deployerProxyAddress)
	if err != nil {
		panic(err)
	}
	proxyArgs := abi.Arguments{
		abi.Argument{Type: mustNewType("address")},
		abi.Argument{Type: mustNewType("bytes")},
		abi.Argument{Type: mustNewType("bytes")},
	}
	runtimeProxyConstructor, err := proxyArgs.Pack(
		// address of runtime upgrade that can do future upgrades
		runtimeUpgradeAddress,
		// bytecode of the default implementation system smart contract
		append(byteCodeFromArtifact(rawArtifact), constructorArgs...),
		// initializer for system smart contract (it's called using "init()" function)
		createInitializer(initTypes, initArgs),
	)
	if err != nil {
		panic(err)
	}
	return append(byteCodeFromArtifact(runtimeProxyArtifact), runtimeProxyConstructor...)
}

func createVirtualMachine(genesis *core.Genesis, systemContract common.Address, balance *big.Int) (*state.StateDB, *vm.EVM) {
	statedb, _ := state.New(common.Hash{}, state.NewDatabaseWithConfig(rawdb.NewDatabase(memorydb.New()), &trie.Config{}), nil)
	if balance != nil {
		statedb.SetBalance(systemContract, balance)
	}
	block := genesis.ToBlock(nil)
	blockContext := core.NewEVMBlockContext(block.Header(), &dummyChainContext{}, &common.Address{})
	txContext := core.NewEVMTxContext(
		types.NewMessage(common.Address{}, &systemContract, 0, big.NewInt(0), 10_000_000, big.NewInt(0), []byte{}, nil, false),
	)
	return statedb, vm.NewEVM(blockContext, txContext, statedb, genesis.Config, vm.Config{})
}

func invokeConstructorOrPanic(genesis *core.Genesis, systemContract common.Address, rawArtifact []byte, typeNames []string, params []interface{}, balance *big.Int) {
	if balance == nil {
		balance = big.NewInt(0)
	}
	statedb, virtualMachine := createVirtualMachine(genesis, systemContract, balance)
	var bytecode []byte
	if systemContract == runtimeUpgradeAddress {
		bytecode = createSimpleBytecode(rawArtifact)
	} else {
		bytecode = createProxyBytecodeWithConstructor(rawArtifact, typeNames, params)
	}
	result, _, err := virtualMachine.CreateWithAddress(vm.AccountRef(common.Address{}), bytecode, 10_000_000, big.NewInt(0), systemContract)
	if err != nil {
		traceCallError(result)
		panic(err)
	}
	if genesis.Alloc == nil {
		genesis.Alloc = make(core.GenesisAlloc)
	}
	// constructor might have side effects so better to save all state changes
	stateObjects := readStateObjectsFromState(statedb)
	for addr, stateObject := range stateObjects {
		storage := readDirtyStorageFromState(stateObject)
		genesisAccount := core.GenesisAccount{
			Code:    stateObject.Code(statedb.Database()),
			Storage: storage.Copy(),
			Balance: stateObject.Balance(),
		}
		genesis.Alloc[addr] = genesisAccount
	}
	// someone touches zero address and it increases nonce
	delete(genesis.Alloc, common.Address{})
}

func createGenesisConfig(config genesisConfig, targetFile string) ([]byte, error) {
	genesis := defaultGenesisConfig(config)
	if len(config.Owners) == 0 {
		config.Owners = config.Validators
	}
	// extra data
	genesis.ExtraData = createExtraData(config.Validators)
	genesis.Config.Parlia.Epoch = uint64(config.ConsensusParams.EpochBlockInterval)
	// execute system contracts
	var initialStakes []*big.Int
	initialStakeTotal := big.NewInt(0)
	for _, v := range config.Validators {
		rawInitialStake, ok := config.InitialStakes[v]
		if !ok {
			return nil, fmt.Errorf("initial stake is not found for validator: %s", v.Hex())
		}
		initialStake, err := hexutil.DecodeBig(rawInitialStake)
		if err != nil {
			return nil, err
		}
		initialStakes = append(initialStakes, initialStake)
		initialStakeTotal.Add(initialStakeTotal, initialStake)
	}
	invokeConstructorOrPanic(genesis, stakingAddress, stakingRawArtifact, []string{"address[]", "address[]", "uint256[]", "uint16"}, []interface{}{
		config.Validators,
		config.Owners,
		initialStakes,
		uint16(config.CommissionRate),
	}, initialStakeTotal)
	invokeConstructorOrPanic(genesis, chainConfigAddress, chainConfigRawArtifact, []string{"uint32", "uint32", "uint32", "uint32", "uint32", "uint32", "uint256", "uint256"}, []interface{}{
		config.ConsensusParams.ActiveValidatorsLength,
		config.ConsensusParams.EpochBlockInterval,
		config.ConsensusParams.MisdemeanorThreshold,
		config.ConsensusParams.FelonyThreshold,
		config.ConsensusParams.ValidatorJailEpochLength,
		config.ConsensusParams.UndelegatePeriod,
		(*big.Int)(config.ConsensusParams.MinValidatorStakeAmount),
		(*big.Int)(config.ConsensusParams.MinStakingAmount),
	}, nil)
	invokeConstructorOrPanic(genesis, slashingIndicatorAddress, slashingIndicatorRawArtifact, []string{}, []interface{}{}, nil)
	invokeConstructorOrPanic(genesis, stakingPoolAddress, stakingPoolRawArtifact, []string{}, []interface{}{}, nil)
	var treasuryAddresses []common.Address
	var treasuryShares []uint16
	for k, v := range config.SystemTreasury {
		treasuryAddresses = append(treasuryAddresses, k)
		treasuryShares = append(treasuryShares, v)
	}
	invokeConstructorOrPanic(genesis, systemRewardAddress, systemRewardRawArtifact, []string{"address[]", "uint16[]"}, []interface{}{
		treasuryAddresses, treasuryShares,
	}, nil)
	invokeConstructorOrPanic(genesis, governanceAddress, governanceRawArtifact, []string{"uint256", "string"}, []interface{}{
		big.NewInt(config.VotingPeriod), "Governance",
	}, nil)
	invokeConstructorOrPanic(genesis, runtimeUpgradeAddress, runtimeUpgradeRawArtifact, []string{"address"}, []interface{}{
		systemcontract.EvmHookRuntimeUpgradeAddress,
	}, nil)
	invokeConstructorOrPanic(genesis, deployerProxyAddress, deployerProxyRawArtifact, []string{"address[]"}, []interface{}{
		config.Deployers,
	}, nil)
	// create system contract
	genesis.Alloc[intermediarySystemAddress] = core.GenesisAccount{
		Balance: big.NewInt(0),
	}
	// apply faucet
	for key, value := range config.Faucet {
		balance, ok := new(big.Int).SetString(value[2:], 16)
		if !ok {
			return nil, fmt.Errorf("failed to parse number (%s)", value)
		}
		genesis.Alloc[key] = core.GenesisAccount{
			Balance: balance,
		}
	}
	// save to file
	newJson, _ := json.MarshalIndent(genesis, "", "  ")
	if targetFile == "stdout" {
		_, err := os.Stdout.Write(newJson)
		return newJson, err
	} else if targetFile == "stderr" {
		_, err := os.Stderr.Write(newJson)
		return newJson, err
	}
	return newJson, ioutil.WriteFile(targetFile, newJson, fs.ModePerm)
}

func defaultGenesisConfig(config genesisConfig) *core.Genesis {
	chainConfig := &params.ChainConfig{
		ChainID:             big.NewInt(config.ChainId),
		HomesteadBlock:      big.NewInt(0),
		EIP150Block:         big.NewInt(0),
		EIP155Block:         big.NewInt(0),
		EIP158Block:         big.NewInt(0),
		ByzantiumBlock:      big.NewInt(0),
		ConstantinopleBlock: big.NewInt(0),
		PetersburgBlock:     big.NewInt(0),
		IstanbulBlock:       big.NewInt(0),
		MuirGlacierBlock:    big.NewInt(0),
		RamanujanBlock:      big.NewInt(0),
		NielsBlock:          big.NewInt(0),
		MirrorSyncBlock:     big.NewInt(0),
		BrunoBlock:          big.NewInt(0),
		Parlia: &params.ParliaConfig{
			Period: 3,
			// epoch length is managed by consensus params
		},
	}
	// by default runtime upgrades are disabled
	if config.Features.RuntimeUpgradeBlock != nil {
		chainConfig.RuntimeUpgradeBlock = (*big.Int)(config.Features.RuntimeUpgradeBlock)
	}
	return &core.Genesis{
		Config:     chainConfig,
		Nonce:      0,
		Timestamp:  0x5e9da7ce,
		ExtraData:  nil,
		GasLimit:   0x2625a00,
		Difficulty: big.NewInt(0x01),
		Mixhash:    common.Hash{},
		Coinbase:   common.Address{},
		Alloc:      nil,
		Number:     0x00,
		GasUsed:    0x00,
		ParentHash: common.Hash{},
	}
}

var localNetConfig = genesisConfig{
	ChainId: 1337,
	// who is able to deploy smart contract from genesis block
	Deployers: []common.Address{
		common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"),
	},
	// list of default validators
	Validators: []common.Address{
		common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"),
	},
	SystemTreasury: map[common.Address]uint16{
		common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"): 10000,
	},
	ConsensusParams: consensusParams{
		ActiveValidatorsLength:   1,
		EpochBlockInterval:       100,
		MisdemeanorThreshold:     10,
		FelonyThreshold:          100,
		ValidatorJailEpochLength: 1,
		UndelegatePeriod:         0,
		MinValidatorStakeAmount:  (*math.HexOrDecimal256)(hexutil.MustDecodeBig("0xde0b6b3a7640000")), // 1 ether
		MinStakingAmount:         (*math.HexOrDecimal256)(hexutil.MustDecodeBig("0xde0b6b3a7640000")), // 1 ether
	},
	InitialStakes: map[common.Address]string{
		common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"): "0x3635c9adc5dea00000", // 1000 eth
	},
	// owner of the governance
	VotingPeriod: 20, // 1 minute
	// faucet
	Faucet: map[common.Address]string{
		common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"): "0x21e19e0c9bab2400000",
		common.HexToAddress("0x57BA24bE2cF17400f37dB3566e839bfA6A2d018a"): "0x21e19e0c9bab2400000",
		common.HexToAddress("0xEbCf9D06cf9333706E61213F17A795B2F7c55F1b"): "0x21e19e0c9bab2400000",
	},
}

var devNetConfig = genesisConfig{
	ChainId: 14000,
	// who is able to deploy smart contract from genesis block (it won't generate event log)
	Deployers: []common.Address{},
	// list of default validators (it won't generate event log)
	Validators: []common.Address{
		common.HexToAddress("0x08fae3885e299c24ff9841478eb946f41023ac69"),
		common.HexToAddress("0x751aaca849b09a3e347bbfe125cf18423cc24b40"),
		common.HexToAddress("0xa6ff33e3250cc765052ac9d7f7dfebda183c4b9b"),
		common.HexToAddress("0x49c0f7c8c11a4c80dc6449efe1010bb166818da8"),
		common.HexToAddress("0x8e1ea6eaa09c3b40f4a51fcd056a031870a0549a"),
	},
	SystemTreasury: map[common.Address]uint16{
		common.HexToAddress("0x0000000000000000000000000000000000000000"): 10000,
	},
	ConsensusParams: consensusParams{
		ActiveValidatorsLength:   25,   // suggested values are (3k+1, where k is honest validators, even better): 7, 13, 19, 25, 31...
		EpochBlockInterval:       1200, // better to use 1 day epoch (86400/3=28800, where 3s is block time)
		MisdemeanorThreshold:     50,   // after missing this amount of blocks per day validator losses all daily rewards (penalty)
		FelonyThreshold:          150,  // after missing this amount of blocks per day validator goes in jail for N epochs
		ValidatorJailEpochLength: 7,    // how many epochs validator should stay in jail (7 epochs = ~7 days)
		UndelegatePeriod:         6,    // allow claiming funds only after 6 epochs (~7 days)

		MinValidatorStakeAmount: (*math.HexOrDecimal256)(hexutil.MustDecodeBig("0xde0b6b3a7640000")), // how many tokens validator must stake to create a validator (in ether)
		MinStakingAmount:        (*math.HexOrDecimal256)(hexutil.MustDecodeBig("0xde0b6b3a7640000")), // minimum staking amount for delegators (in ether)
	},
	InitialStakes: map[common.Address]string{
		common.HexToAddress("0x08fae3885e299c24ff9841478eb946f41023ac69"): "0x3635c9adc5dea00000", // 1000 eth
		common.HexToAddress("0x751aaca849b09a3e347bbfe125cf18423cc24b40"): "0x3635c9adc5dea00000", // 1000 eth
		common.HexToAddress("0xa6ff33e3250cc765052ac9d7f7dfebda183c4b9b"): "0x3635c9adc5dea00000", // 1000 eth
		common.HexToAddress("0x49c0f7c8c11a4c80dc6449efe1010bb166818da8"): "0x3635c9adc5dea00000", // 1000 eth
		common.HexToAddress("0x8e1ea6eaa09c3b40f4a51fcd056a031870a0549a"): "0x3635c9adc5dea00000", // 1000 eth
	},
	// owner of the governance
	VotingPeriod: 60, // 3 minutes
	// faucet
	Faucet: map[common.Address]string{
		common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"): "0x21e19e0c9bab2400000",    // governance
		common.HexToAddress("0xb891fe7b38f857f53a7b5529204c58d5c487280b"): "0x52b7d2dcc80cd2e4000000", // faucet (10kk)
	},
}

func returnError(writer http.ResponseWriter, err error) {
	writer.WriteHeader(500)
	_, _ = writer.Write([]byte(err.Error()))
}

func handleCorsRequest(w http.ResponseWriter, r *http.Request) bool {
	var origin string
	if origin = r.Header.Get("Origin"); origin == "" {
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Credentials", "true")
	if r.Method != "OPTIONS" || r.Header.Get("Access-Control-Request-Method") == "" {
		return false
	}
	headers := []string{"Content-Type", "Accept"}
	w.Header().Set("Access-Control-Allow-Headers", strings.Join(headers, ","))
	methods := []string{"GET", "HEAD", "POST", "PUT", "DELETE"}
	w.Header().Set("Access-Control-Allow-Methods", strings.Join(methods, ","))
	return true
}

func httpRpcServer() {
	r := mux.NewRouter()
	r.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				returnError(writer, err.(error))
				return
			}
		}()
		if handleCorsRequest(writer, request) {
			return
		}
		input, err := ioutil.ReadAll(request.Body)
		if err != nil {
			returnError(writer, err)
			return
		}
		genesis := &genesisConfig{}
		err = json.Unmarshal(input, genesis)
		if err != nil {
			returnError(writer, err)
			return
		}
		result, err := createGenesisConfig(*genesis, "stdout")
		if err != nil {
			returnError(writer, err)
			return
		}
		_, _ = writer.Write(result)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(200)
	})
	if err := http.ListenAndServe(":8080", r); err != nil {
		panic(err)
	}
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--http" {
		httpRpcServer()
		return
	}
	if len(args) > 0 {
		fileContents, err := os.ReadFile(args[0])
		if err != nil {
			panic(err)
		}
		genesis := &genesisConfig{}
		err = json.Unmarshal(fileContents, genesis)
		if err != nil {
			panic(err)
		}
		outputFile := "stdout"
		if len(args) > 1 {
			outputFile = args[1]
		}
		_, err = createGenesisConfig(*genesis, outputFile)
		if err != nil {
			panic(err)
		}
		return
	}
	fmt.Printf("building local net\n")
	if _, err := createGenesisConfig(localNetConfig, "localnet.json"); err != nil {
		panic(err)
	}
	fmt.Printf("\nbuilding dev net\n")
	if _, err := createGenesisConfig(devNetConfig, "devnet.json"); err != nil {
		panic(err)
	}
	fmt.Printf("\n")
}
