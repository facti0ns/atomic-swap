package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/ChainSafe/chaindb"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/urfave/cli/v2"

	"github.com/athanorlabs/atomic-swap/cliutil"
	"github.com/athanorlabs/atomic-swap/common"
	"github.com/athanorlabs/atomic-swap/db"
	"github.com/athanorlabs/atomic-swap/monero"
	"github.com/athanorlabs/atomic-swap/net"
	"github.com/athanorlabs/atomic-swap/protocol/backend"
	"github.com/athanorlabs/atomic-swap/protocol/swap"
	"github.com/athanorlabs/atomic-swap/protocol/xmrmaker"
	"github.com/athanorlabs/atomic-swap/protocol/xmrtaker"
	"github.com/athanorlabs/atomic-swap/rpc"

	logging "github.com/ipfs/go-log"
)

const (
	// default libp2p ports
	defaultLibp2pPort         = 9900
	defaultXMRTakerLibp2pPort = 9933
	defaultXMRMakerLibp2pPort = 9934

	// default RPC port
	defaultRPCPort         = 5005
	defaultXMRTakerRPCPort = 5001
	defaultXMRMakerRPCPort = 5002
)

var (
	log = logging.Logger("cmd")

	// Default dev base paths. If SWAP_TEST_DATA_DIR is not defined, it is
	// still safe, there just won't be an intermediate directory and tests
	// could fail from stale data.
	testDataDir = os.Getenv("SWAP_TEST_DATA_DIR")
	// MkdirTemp uses os.TempDir() by default if the first argument is an empty string.
	defaultXMRMakerDataDir, _ = os.MkdirTemp("", path.Join(testDataDir, "xmrmaker-*"))
	defaultXMRTakerDataDir, _ = os.MkdirTemp("", path.Join(testDataDir, "xmrtaker-*"))
)

const (
	flagRPCPort    = "rpc-port"
	flagDataDir    = "data-dir"
	flagLibp2pKey  = "libp2p-key"
	flagLibp2pPort = "libp2p-port"
	flagBootnodes  = "bootnodes"

	flagEnv                  = "env"
	flagMoneroDaemonHost     = "monerod-host"
	flagMoneroDaemonPort     = "monerod-port"
	flagMoneroWalletPath     = "wallet-file"
	flagMoneroWalletPassword = "wallet-password"
	flagMoneroWalletPort     = "wallet-port"
	flagEthereumEndpoint     = "ethereum-endpoint"
	flagEthereumPrivKey      = "ethereum-privkey"
	flagContractAddress      = "contract-address"
	flagGasPrice             = "gas-price"
	flagGasLimit             = "gas-limit"
	flagUseExternalSigner    = "external-signer"

	flagDevXMRTaker  = "dev-xmrtaker"
	flagDevXMRMaker  = "dev-xmrmaker"
	flagDeploy       = "deploy"
	flagTransferBack = "transfer-back"

	flagLogLevel = "log-level"
)

var (
	app = &cli.App{
		Name:                 "swapd",
		Usage:                "A program for doing atomic swaps between ETH and XMR",
		Version:              cliutil.GetVersion(),
		Action:               runDaemon,
		EnableBashCompletion: true,
		Suggest:              true,
		Flags: []cli.Flag{
			&cli.UintFlag{
				Name:  flagRPCPort,
				Usage: "Port for the daemon RPC server to run on",
				Value: defaultRPCPort,
			},
			&cli.StringFlag{
				Name:  flagDataDir,
				Usage: "Path to store swap artifacts", //nolint:misspell
				Value: "{HOME}/.atomicswap/{ENV}",     // For --help only, actual default replaces variables
			},
			&cli.StringFlag{
				Name:  flagLibp2pKey,
				Usage: "libp2p private key",
				Value: fmt.Sprintf("{DATA_DIR}/%s", common.DefaultLibp2pKeyFileName),
			},
			&cli.UintFlag{
				Name:  flagLibp2pPort,
				Usage: "libp2p port to listen on",
				Value: defaultLibp2pPort,
			},
			&cli.StringFlag{
				Name:  flagEnv,
				Usage: "Environment to use: one of mainnet, stagenet, or dev",
				Value: "dev",
			},
			&cli.StringFlag{
				Name:  flagMoneroDaemonHost,
				Usage: "monerod host",
				Value: "127.0.0.1",
			},
			&cli.UintFlag{
				Name: flagMoneroDaemonPort,
				Usage: fmt.Sprintf("monerod port (--%s=stagenet changes default to %d)",
					flagEnv, common.DefaultMoneroDaemonStagenetPort),
				Value: common.DefaultMoneroDaemonMainnetPort, // at least for now, this is also the dev default
			},
			&cli.StringFlag{
				Name:  flagMoneroWalletPath,
				Usage: "Path to the Monero wallet file, created if missing",
				Value: fmt.Sprintf("{DATA-DIR}/wallet/%s", common.DefaultMoneroWalletName),
			},
			&cli.StringFlag{
				Name:  flagMoneroWalletPassword,
				Usage: "Password of monero wallet file",
			},
			&cli.UintFlag{
				Name:   flagMoneroWalletPort,
				Usage:  "The port that the internal monero-wallet-rpc instance listens on",
				Hidden: true, // flag is for integration tests and won't be supported long term
			},
			&cli.StringFlag{
				Name:  flagEthereumEndpoint,
				Usage: "Ethereum client endpoint",
			},
			&cli.StringFlag{
				Name:  flagEthereumPrivKey,
				Usage: "File containing ethereum private key as hex, new key is generated if missing",
				Value: fmt.Sprintf("{DATA-DIR}/%s", common.DefaultEthKeyFileName),
			},
			&cli.StringFlag{
				Name:  flagContractAddress,
				Usage: "Address of instance of SwapFactory.sol already deployed on-chain; required if running on mainnet",
			},
			&cli.StringSliceFlag{
				Name:    flagBootnodes,
				Aliases: []string{"bn"},
				Usage:   "libp2p bootnode, comma separated if passing multiple to a single flag",
			},
			&cli.UintFlag{
				Name:  flagGasPrice,
				Usage: "Ethereum gas price to use for transactions (in gwei). If not set, the gas price is set via oracle.",
			},
			&cli.UintFlag{
				Name:  flagGasLimit,
				Usage: "Ethereum gas limit to use for transactions. If not set, the gas limit is estimated for each transaction.",
			},
			&cli.BoolFlag{
				Name:  flagDevXMRTaker,
				Usage: "Run in development mode and use ETH provider default values",
			},
			&cli.BoolFlag{
				Name:  flagDevXMRMaker,
				Usage: "Run in development mode and use XMR provider default values",
			},
			&cli.BoolFlag{
				Name:  flagDeploy,
				Usage: "Deploy an instance of the swap contract",
			},
			&cli.BoolFlag{
				Name:  flagTransferBack,
				Usage: "When receiving XMR in a swap, transfer it back to the original wallet.",
			},
			&cli.StringFlag{
				Name:  flagLogLevel,
				Usage: "Set log level: one of [error|warn|info|debug]",
				Value: "info",
			},
			&cli.BoolFlag{
				Name:  flagUseExternalSigner,
				Usage: "Use external signer, for usage with the swap UI",
			},
		},
	}
)

func main() {
	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
		os.Exit(1)
	}
}

type xmrtakerHandler interface {
	rpc.XMRTaker
}

type xmrmakerHandler interface {
	net.Handler
	rpc.XMRMaker
}

type daemon struct {
	ctx       context.Context
	cancel    context.CancelFunc
	database  *db.Database
	host      net.Host
	rpcServer *rpc.Server
}

func setLogLevelsFromContext(c *cli.Context) error {
	const (
		levelError = "error"
		levelWarn  = "warn"
		levelInfo  = "info"
		levelDebug = "debug"
	)

	level := c.String(flagLogLevel)
	switch level {
	case levelError, levelWarn, levelInfo, levelDebug:
	default:
		return fmt.Errorf("invalid log level %q", level)
	}

	setLogLevels(level)
	return nil
}

func setLogLevels(level string) {
	_ = logging.SetLogLevel("xmrtaker", level)
	_ = logging.SetLogLevel("xmrmaker", level)
	_ = logging.SetLogLevel("common", level)
	_ = logging.SetLogLevel("cmd", level)
	_ = logging.SetLogLevel("net", level)
	_ = logging.SetLogLevel("offers", level)
	_ = logging.SetLogLevel("rpc", level)
	_ = logging.SetLogLevel("monero", level)
}

func runDaemon(c *cli.Context) error {
	if err := setLogLevelsFromContext(c); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := &daemon{
		ctx:    ctx,
		cancel: cancel,
	}

	go func() {
		err := d.make(c)
		if err != nil {
			log.Errorf("failed to make daemon: %s", err)
			cancel()
		}
	}()

	d.wait()

	err := d.stop()
	if err != nil {
		log.Warnf("failed to gracefully stop daemon: %s", err)
	}

	return nil
}

func (d *daemon) stop() error {
	err := d.database.Close()
	if err != nil {
		return err
	}

	err = d.host.Stop()
	if err != nil {
		return err
	}

	err = d.rpcServer.Stop()
	if err != nil {
		return err
	}

	return nil
}

// expandBootnodes expands the boot nodes passed on the command line that
// can be specified individually with multiple flags, but can also contain
// multiple boot nodes passed to single flag separated by commas.
func expandBootnodes(nodesCLI []string) []string {
	var nodes []string
	for _, n := range nodesCLI {
		splitNodes := strings.Split(n, ",")
		for _, ns := range splitNodes {
			nodes = append(nodes, strings.TrimSpace(ns))
		}
	}
	return nodes
}

func (d *daemon) make(c *cli.Context) error {
	env, cfg, err := cliutil.GetEnvironment(c.String(flagEnv))
	if err != nil {
		return err
	}

	devXMRMaker := c.Bool(flagDevXMRMaker)
	devXMRTaker := c.Bool(flagDevXMRTaker)
	if devXMRMaker && devXMRTaker {
		return errFlagsMutuallyExclusive(flagDevXMRMaker, flagDevXMRTaker)
	}

	// cfg.DataDir already has a default set, so only override if the user explicitly set the flag
	if c.IsSet(flagDataDir) {
		cfg.DataDir = c.String(flagDataDir) // override the value derived from `flagEnv`
		if cfg.DataDir == "" {
			return errFlagValueEmpty(flagDataDir)
		}
	} else if env == common.Development {
		// Override in dev scenarios if the value was not explicitly set
		switch {
		case devXMRTaker:
			cfg.DataDir = defaultXMRTakerDataDir
		case devXMRMaker:
			cfg.DataDir = defaultXMRMakerDataDir
		}
	}
	if err = common.MakeDir(cfg.DataDir); err != nil {
		return err
	}

	if len(c.StringSlice(flagBootnodes)) > 0 {
		cfg.Bootnodes = expandBootnodes(c.StringSlice(flagBootnodes))
	}

	libp2pKey := cfg.LibP2PKeyFile()
	if c.IsSet(flagLibp2pKey) {
		libp2pKey = c.String(flagLibp2pKey)
		if libp2pKey == "" {
			return errFlagValueEmpty(flagLibp2pKey)
		}
	}

	libp2pPort := uint16(c.Uint(flagLibp2pPort))
	if !c.IsSet(flagLibp2pPort) {
		switch {
		case devXMRTaker:
			libp2pPort = defaultXMRTakerLibp2pPort
		case devXMRMaker:
			libp2pPort = defaultXMRMakerLibp2pPort
		}
	}

	ethEndpoint := common.DefaultEthEndpoint
	if c.String(flagEthereumEndpoint) != "" {
		ethEndpoint = c.String(flagEthereumEndpoint)
	}
	ec, err := ethclient.Dial(ethEndpoint)
	if err != nil {
		return err
	}
	chainID, err := ec.ChainID(d.ctx)
	if err != nil {
		return err
	}

	netCfg := &net.Config{
		Ctx:         d.ctx,
		Environment: env,
		DataDir:     cfg.DataDir,
		EthChainID:  chainID.Int64(),
		Port:        libp2pPort,
		KeyFile:     libp2pKey,
		Bootnodes:   cfg.Bootnodes,
	}
	host, err := net.NewHost(netCfg)
	if err != nil {
		return err
	}
	d.host = host

	dbCfg := &chaindb.Config{
		DataDir: path.Join(cfg.DataDir, "db"),
	}

	db, err := db.NewDatabase(dbCfg)
	if err != nil {
		return err
	}
	d.database = db

	sm := swap.NewManager()
	backend, err := newBackend(d.ctx, c, env, cfg, devXMRMaker, devXMRTaker, sm, host, ec)
	if err != nil {
		return err
	}
	defer backend.Close()
	log.Infof("created backend with monero endpoint %s and ethereum endpoint %s", backend.Endpoint(), ethEndpoint)

	a, b, err := getProtocolInstances(c, cfg, backend, db, host)
	if err != nil {
		return err
	}

	// connect network to protocol handler
	// handler handles initiated ("taken") swap
	host.SetHandler(b)

	if err = host.Start(); err != nil {
		return err
	}

	rpcPort := uint16(c.Uint(flagRPCPort))
	if !c.IsSet(flagRPCPort) {
		switch {
		case devXMRTaker:
			rpcPort = defaultXMRTakerRPCPort
		case devXMRMaker:
			rpcPort = defaultXMRMakerRPCPort
		}
	}
	listenAddr := fmt.Sprintf("127.0.0.1:%d", rpcPort)

	rpcCfg := &rpc.Config{
		Ctx:             d.ctx,
		Address:         listenAddr,
		Net:             host,
		XMRTaker:        a,
		XMRMaker:        b,
		ProtocolBackend: backend,
	}

	s, err := rpc.NewServer(rpcCfg)
	if err != nil {
		return err
	}
	d.rpcServer = s

	log.Infof("starting swapd with data-dir %s", cfg.DataDir)
	err = s.Start()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

func errFlagsMutuallyExclusive(flag1, flag2 string) error {
	return fmt.Errorf("flags %q and %q are mutually exclusive", flag1, flag2)
}

func errFlagValueEmpty(flag string) error {
	return fmt.Errorf("flag %q requires a non-empty value", flag)
}

func newBackend(
	ctx context.Context,
	c *cli.Context,
	env common.Environment,
	cfg common.Config,
	devXMRMaker bool,
	devXMRTaker bool,
	sm swap.Manager,
	net net.Host,
	ec *ethclient.Client,
) (backend.Backend, error) {
	var (
		ethPrivKey *ecdsa.PrivateKey
	)

	useExternalSigner := c.Bool(flagUseExternalSigner)
	if useExternalSigner && c.IsSet(flagEthereumPrivKey) {
		return nil, errFlagsMutuallyExclusive(flagUseExternalSigner, flagEthereumPrivKey)
	}

	if !useExternalSigner {
		ethPrivKeyFile := cfg.EthKeyFileName()
		if c.IsSet(flagEthereumPrivKey) {
			ethPrivKeyFile = c.String(flagEthereumPrivKey)
			if ethPrivKeyFile == "" {
				return nil, errFlagValueEmpty(flagEthereumPrivKey)
			}
		}
		var err error
		if ethPrivKey, err = cliutil.GetEthereumPrivateKey(ethPrivKeyFile, env, devXMRMaker, devXMRTaker); err != nil {
			return nil, err
		}
	}

	// TODO: add configs for different eth testnets + L2 and set gas limit based on those, if not set (#153)
	var gasPrice *big.Int
	if c.Uint(flagGasPrice) != 0 {
		gasPrice = big.NewInt(int64(c.Uint(flagGasPrice)))
	}

	deploy := c.Bool(flagDeploy)
	if deploy {
		if c.IsSet(flagContractAddress) {
			return nil, errFlagsMutuallyExclusive(flagDeploy, flagContractAddress)
		}
		// Zero out any default contract address in the config, so we deploy
		cfg.ContractAddress = ethcommon.Address{}
	} else {
		contractAddrStr := c.String(flagContractAddress)
		if contractAddrStr != "" {
			if !ethcommon.IsHexAddress(contractAddrStr) {
				return nil, fmt.Errorf("%q is not a valid contract address", contractAddrStr)
			}
			cfg.ContractAddress = ethcommon.HexToAddress(contractAddrStr)
		}
		if bytes.Equal(cfg.ContractAddress.Bytes(), ethcommon.Address{}.Bytes()) {
			return nil, fmt.Errorf("flag %q or %q is required for env=%s", flagDeploy, flagContractAddress, env)
		}
	}

	contract, contractAddr, err := getOrDeploySwapFactory(ctx, cfg.ContractAddress, env, cfg.DataDir, ethPrivKey, ec)
	if err != nil {
		return nil, err
	}

	// For the monero wallet related values, keep the default config values unless the end
	// use explicitly set the flag.
	if c.IsSet(flagMoneroDaemonHost) {
		cfg.MoneroDaemonHost = c.String(flagMoneroDaemonHost)
		if cfg.MoneroDaemonHost == "" {
			return nil, errFlagValueEmpty(flagMoneroDaemonHost)
		}
	}
	if c.IsSet(flagMoneroDaemonPort) {
		cfg.MoneroDaemonPort = c.Uint(flagMoneroDaemonPort)
	}
	walletFilePath := cfg.MoneroWalletPath()
	if c.IsSet(flagMoneroWalletPath) {
		walletFilePath = c.String(flagMoneroWalletPath)
		if walletFilePath == "" {
			return nil, errFlagValueEmpty(flagMoneroWalletPath)
		}
	}
	mc, err := monero.NewWalletClient(&monero.WalletClientConf{
		Env:                 env,
		WalletFilePath:      walletFilePath,
		MonerodPort:         cfg.MoneroDaemonPort,
		MonerodHost:         cfg.MoneroDaemonHost,
		MoneroWalletRPCPath: "", // look for it in "monero-bin/monero-wallet-rpc" and then the user's path
		WalletPassword:      c.String(flagMoneroWalletPassword),
		WalletPort:          c.Uint(flagMoneroWalletPort),
	})
	if err != nil {
		return nil, err
	}

	bcfg := &backend.Config{
		Ctx:                 ctx,
		MoneroClient:        mc,
		EthereumClient:      ec,
		EthereumPrivateKey:  ethPrivKey,
		Environment:         env,
		GasPrice:            gasPrice,
		GasLimit:            uint64(c.Uint(flagGasLimit)),
		SwapManager:         sm,
		SwapContract:        contract,
		SwapContractAddress: contractAddr,
		Net:                 net,
	}

	b, err := backend.NewBackend(bcfg)
	if err != nil {
		mc.Close()
		return nil, fmt.Errorf("failed to make backend: %w", err)
	}

	return b, nil
}

func getProtocolInstances(c *cli.Context, cfg common.Config,
	b backend.Backend, db *db.Database, host net.Host) (xmrtakerHandler, xmrmakerHandler, error) {
	walletFilePath := cfg.MoneroWalletPath()
	if c.IsSet(flagMoneroWalletPath) {
		walletFilePath = c.String(flagMoneroWalletPath)
		if walletFilePath == "" {
			return nil, nil, errFlagValueEmpty(flagMoneroWalletPath)
		}
	}

	// empty password is ok
	walletPassword := c.String(flagMoneroWalletPassword)

	xmrtakerCfg := &xmrtaker.Config{
		Backend:              b,
		DataDir:              cfg.DataDir,
		MoneroWalletFile:     walletFilePath,
		MoneroWalletPassword: walletPassword,
		TransferBack:         c.Bool(flagTransferBack),
	}

	xmrtaker, err := xmrtaker.NewInstance(xmrtakerCfg)
	if err != nil {
		return nil, nil, err
	}

	xmrmakerCfg := &xmrmaker.Config{
		Backend:        b,
		DataDir:        cfg.DataDir,
		Database:       db,
		WalletFile:     walletFilePath,
		WalletPassword: walletPassword,
		Network:        host,
	}

	xmrmaker, err := xmrmaker.NewInstance(xmrmakerCfg)
	if err != nil {
		return nil, nil, err
	}

	return xmrtaker, xmrmaker, nil
}