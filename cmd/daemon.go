package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/filecoin-project/venus/fixtures/assets"
	"github.com/filecoin-project/venus/fixtures/networks"
	"github.com/filecoin-project/venus/venus-shared/actors"
	types2 "github.com/filecoin-project/venus/venus-shared/actors/types"
	"github.com/filecoin-project/venus/venus-shared/utils"

	"github.com/filecoin-project/venus/pkg/chainsync/slashfilter"
	"github.com/filecoin-project/venus/pkg/util/ulimit"

	paramfetch "github.com/filecoin-project/go-paramfetch"

	_ "net/http/pprof" // nolint: golint

	cmds "github.com/ipfs/go-ipfs-cmds"
	logging "github.com/ipfs/go-log/v2"

	"github.com/filecoin-project/venus/app/node"
	"github.com/filecoin-project/venus/app/paths"
	"github.com/filecoin-project/venus/pkg/config"
	"github.com/filecoin-project/venus/pkg/genesis"
	"github.com/filecoin-project/venus/pkg/journal"
	"github.com/filecoin-project/venus/pkg/migration"
	"github.com/filecoin-project/venus/pkg/repo"
)

var log = logging.Logger("daemon")

const (
	makeGenFlag     = "make-genesis"
	preTemplateFlag = "genesis-template"
)

var daemonCmd = &cmds.Command{
	Helptext: cmds.HelpText{
		Tagline: "Initialize a venus repo, Start a long-running daemon process",
	},
	Options: []cmds.Option{
		cmds.StringOption(makeGenFlag, "make genesis"),
		cmds.StringOption(preTemplateFlag, "template for make genesis"),
		cmds.StringOption(SwarmAddress, "multiaddress to listen on for filecoin network connections"),
		cmds.StringOption(SwarmPublicRelayAddress, "public multiaddress for routing circuit relay traffic.  Necessary for relay nodes to provide this if they are not publically dialable"),
		cmds.BoolOption(OfflineMode, "start the node without networking"),
		cmds.BoolOption(ELStdout),
		cmds.BoolOption(ULimit, "manage open file limit").WithDefault(true),
		cmds.StringOption(AuthServiceURL, "venus auth service URL"),
		cmds.StringOption(AuthServiceToken, "venus auth service token"),
		cmds.StringsOption(BootstrapPeers, "set the bootstrap peers"),
		cmds.BoolOption(IsRelay, "advertise and allow venus network traffic to be relayed through this node"),
		cmds.StringOption(ImportSnapshot, "import chain state from a given chain export file or url"),
		cmds.StringOption(GenesisFile, "path of file or HTTP(S) URL containing archive of genesis block DAG data"),
		cmds.StringOption(Network, "when set, populates config with network specific parameters, eg. mainnet,2k,calibrationnet,interopnet,butterflynet").WithDefault("mainnet"),
		cmds.StringOption(Password, "set wallet password"),
		cmds.StringOption(Profile, "specify type of node, eg. bootstrapper"),
		cmds.StringOption(WalletGateway, "set sophon gateway url and token, eg. token:url"),
	},
	Run: func(req *cmds.Request, re cmds.ResponseEmitter, env cmds.Environment) error {
		if limit, _ := req.Options[ULimit].(bool); limit {
			if _, _, err := ulimit.ManageFdLimit(); err != nil {
				log.Errorf("setting file descriptor limit: %s", err)
			}
		}

		repoDir, _ := req.Options[OptionRepoDir].(string)
		repoDir, err := paths.GetRepoPath(repoDir)
		if err != nil {
			return err
		}
		ps, err := assets.GetProofParams()
		if err != nil {
			return err
		}
		srs, err := assets.GetSrs()
		if err != nil {
			return err
		}
		if err := paramfetch.GetParams(req.Context, ps, srs, 0); err != nil {
			return fmt.Errorf("fetching proof parameters: %w", err)
		}

		exist, err := repo.Exists(repoDir) //The configuration file and devgen are required for the program to start
		if err != nil {
			return err
		}
		if !exist {
			defer func() {
				if err != nil {
					log.Infof("Failed to initialize venus, cleaning up %s after attempt...", repoDir)
					if err := os.RemoveAll(repoDir); err != nil {
						log.Errorf("Failed to clean up failed repo: %s", err)
					}
				}
			}()
			log.Infof("Initializing repo at '%s'", repoDir)

			if err := re.Emit(repoDir); err != nil {
				return err
			}

			cfg, err := repo.LoadConfig(repoDir) // use exit config, allow user prepare config before
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					log.Infof("config not exist, use default config")
					cfg = config.NewDefaultConfig()
				} else {
					return err
				}
			}
			if err := repo.InitFSRepo(repoDir, repo.LatestVersion, cfg); err != nil {
				return err
			}

			if err = initRun(req, repoDir); err != nil {
				return err
			}
		}

		return daemonRun(req, re)
	},
}

func initRun(req *cmds.Request, repoDir string) error {
	rep, err := getRepo(repoDir)
	if err != nil {
		return err
	}
	// The only error Close can return is that the repo has already been closed.
	defer func() {
		_ = rep.Close()
	}()
	var genesisFunc genesis.InitFunc
	cfg := rep.Config()
	network, _ := req.Options[Network].(string)
	if err := networks.SetConfigFromOptions(cfg, network); err != nil {
		return fmt.Errorf("setting config: %v", err)
	}
	// genesis node
	if mkGen, ok := req.Options[makeGenFlag].(string); ok {
		preTp := req.Options[preTemplateFlag]
		if preTp == nil {
			return fmt.Errorf("must also pass file with genesis template to `--%s`", preTemplateFlag)
		}

		node.SetNetParams(cfg.NetworkParams)
		if err := actors.SetNetworkBundle(int(cfg.NetworkParams.NetworkType)); err != nil {
			return err
		}
		utils.ReloadMethodsMap()

		genesisFunc = genesis.MakeGenesis(req.Context, rep, mkGen, preTp.(string), cfg.NetworkParams.ForkUpgradeParam)
	} else {
		genesisFileSource, _ := req.Options[GenesisFile].(string)
		genesisFunc, err = genesis.LoadGenesis(req.Context, rep, genesisFileSource, network)
		if err != nil {
			return err
		}
	}
	if authServiceURL, ok := req.Options[AuthServiceURL].(string); ok && len(authServiceURL) > 0 {
		cfg.API.VenusAuthURL = authServiceURL
		if authServiceToken, ok := req.Options[AuthServiceToken].(string); ok && len(authServiceToken) > 0 {
			cfg.API.VenusAuthToken = authServiceToken
		} else {
			return fmt.Errorf("must also pass token with venus auth service to `--%s`", AuthServiceToken)
		}
	}
	if walletGateway, ok := req.Options[WalletGateway].(string); ok && len(walletGateway) > 0 {
		cfg.Wallet.GatewayBacked = walletGateway
	}

	if err := rep.ReplaceConfig(cfg); err != nil {
		log.Errorf("Error replacing config %s", err)
		return err
	}

	if err := node.Init(req.Context, rep, genesisFunc); err != nil {
		log.Errorf("Error initializing node %s", err)
		return err
	}

	// import snapshot argument only work when init
	importPath, _ := req.Options[ImportSnapshot].(string)
	if len(importPath) != 0 {
		err := Import(req.Context, rep, importPath)
		if err != nil {
			log.Errorf("failed to import snapshot, import path: %s, error: %s", importPath, err.Error())
			return err
		}
	}

	return nil
}

func daemonRun(req *cmds.Request, re cmds.ResponseEmitter) error {
	repoDir, _ := req.Options[OptionRepoDir].(string)
	rep, err := getRepo(repoDir)
	if err != nil {
		return err
	}

	config := rep.Config()
	if err := networks.SetConfigFromNetworkType(config, config.NetworkParams.NetworkType); err != nil {
		return fmt.Errorf("set config failed %v %v", config.NetworkParams.NetworkType, err)
	}
	log.Infof("network params: %+v", config.NetworkParams)
	log.Infof("upgrade params: %+v", config.NetworkParams.ForkUpgradeParam)

	if err := actors.SetNetworkBundle(int(config.NetworkParams.NetworkType)); err != nil {
		return err
	}
	utils.ReloadMethodsMap()
	types2.SetEip155ChainID(config.NetworkParams.Eip155ChainID)
	log.Infof("Eip155ChainId %v", types2.Eip155ChainID)

	// second highest precedence is env vars.
	if envAPI := os.Getenv("VENUS_API"); envAPI != "" {
		config.API.APIAddress = envAPI
	}

	// highest precedence is cmd line flag.
	if flagAPI, ok := req.Options[OptionAPI].(string); ok && flagAPI != "" {
		config.API.APIAddress = flagAPI
	}

	if swarmAddress, ok := req.Options[SwarmAddress].(string); ok && swarmAddress != "" {
		config.Swarm.Address = swarmAddress
	}

	if publicRelayAddress, ok := req.Options[SwarmPublicRelayAddress].(string); ok && publicRelayAddress != "" {
		config.Swarm.PublicRelayAddress = publicRelayAddress
	}

	if authURL, ok := req.Options[AuthServiceURL].(string); ok && len(authURL) > 0 {
		config.API.VenusAuthURL = authURL
	}
	if authServiceToken, ok := req.Options[AuthServiceToken].(string); ok && len(authServiceToken) > 0 {
		config.API.VenusAuthToken = authServiceToken
	}
	if len(config.API.VenusAuthURL)+len(config.API.VenusAuthToken) > 0 && len(config.API.VenusAuthToken)*len(config.API.VenusAuthURL) == 0 {
		return fmt.Errorf("must set both venus auth service url and token at the same time")
	}
	if walletGateway, ok := req.Options[WalletGateway].(string); ok && len(walletGateway) > 0 {
		config.Wallet.GatewayBacked = walletGateway
	}

	if bootPeers, ok := req.Options[BootstrapPeers].([]string); ok && len(bootPeers) > 0 {
		config.Bootstrap.AddPeers(bootPeers...)
	}

	if profile, ok := req.Options[Profile].(string); ok && len(profile) > 0 {
		if profile != "bootstrapper" {
			return fmt.Errorf("unrecognized profile type: %s", profile)
		}
		config.PubsubConfig.Bootstrapper = true
	}

	opts, err := node.OptionsFromRepo(rep)
	if err != nil {
		return err
	}

	if offlineMode, ok := req.Options[OfflineMode].(bool); ok { // nolint
		opts = append(opts, node.OfflineMode(offlineMode))
	}

	if isRelay, ok := req.Options[IsRelay].(bool); ok && isRelay {
		opts = append(opts, node.IsRelay())
	}

	if password, _ := req.Options[Password].(string); len(password) > 0 {
		opts = append(opts, node.SetWalletPassword([]byte(password)))
	}

	journal, err := journal.NewZapJournal(rep.JournalPath()) // nolint
	if err != nil {
		return err
	}
	opts = append(opts, node.JournalConfigOption(journal))

	// Monkey-patch network parameters option will set package variables during node build
	opts = append(opts, node.MonkeyPatchNetworkParamsOption(config.NetworkParams))

	// Instantiate the node.
	fcn, err := node.New(req.Context, opts...)
	if err != nil {
		return err
	}

	if fcn.OfflineMode() {
		_ = re.Emit("Filecoin node running in offline mode (libp2p is disabled)\n")
	} else {
		_ = re.Emit(fmt.Sprintf("My peer ID is %s\n", fcn.Network().Host.ID().String()))
		for _, a := range fcn.Network().Host.Addrs() {
			_ = re.Emit(fmt.Sprintf("Swarm listening on: %s\n", a))
		}
	}

	if _, ok := req.Options[ELStdout].(bool); ok {
		_ = re.Emit("--" + ELStdout + " option is deprecated\n")
	}

	if config.FaultReporter.EnableConsensusFaultReporter {
		if err := slashfilter.SlashConsensus(req.Context, config.FaultReporter, fcn.Wallet().API(),
			fcn.Chain().API(), fcn.Mpool().API(), fcn.Sync().API()); err != nil {
			return fmt.Errorf("run consensus fault reporter failed: %v", err)
		}
	}

	// Start the node.
	if err := fcn.Start(req.Context); err != nil {
		return err
	}

	// Run API server around the node.
	ready := make(chan interface{}, 1)
	go func() {
		<-ready
		lines := []string{
			fmt.Sprintf("API server listening on %s\n", config.API.APIAddress),
		}
		_ = re.Emit(lines)
	}()

	// The request is expected to remain open so the daemon uses the request context.
	// Pass a new context here if the flow changes such that the command should exit while leaving
	// a forked deamon running.
	return fcn.RunRPCAndWait(req.Context, RootCmdDaemon, ready)
}

func getRepo(repoDir string) (repo.Repo, error) {
	repoDir, err := paths.GetRepoPath(repoDir)
	if err != nil {
		return nil, err
	}
	if err = migration.TryToMigrate(repoDir); err != nil {
		return nil, err
	}
	return repo.OpenFSRepo(repoDir, repo.LatestVersion)
}
