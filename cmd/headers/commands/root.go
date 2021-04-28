package commands

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path"
	"syscall"

	"github.com/c2h5oh/datasize"
	"github.com/ledgerwatch/turbo-geth/cmd/utils"
	"github.com/ledgerwatch/turbo-geth/common/paths"
	"github.com/ledgerwatch/turbo-geth/ethdb"
	"github.com/ledgerwatch/turbo-geth/internal/debug"
	"github.com/ledgerwatch/turbo-geth/log"
	"github.com/spf13/cobra"
)

var (
	sentryAddr   string   // Address of the sentry <host>:<port>
	sentryAddrs  []string // Address of the sentry <host>:<port>
	chaindata    string   // Path to chaindata
	snapshotDir  string
	snapshotMode string
	datadir      string // Path to td working dir
	database     string // Type of database (lmdb or mdbx)
	mapSizeStr   string // Map size for LMDB
)

func init() {
	utils.CobraFlags(rootCmd, append(debug.Flags, utils.MetricFlags...))
}

func rootContext() context.Context {
	return context.Background()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(ch)

		select {
		case <-ch:
			log.Info("Got interrupt, shutting down...")
		case <-ctx.Done():
		}

		cancel()
	}()
	return ctx
}

var rootCmd = &cobra.Command{
	Use:   "headers",
	Short: "headers is Proof Of Concept for new header/block downloading algorithms",
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		if err := debug.SetupCobra(cmd); err != nil {
			panic(err)
		}
		if chaindata == "" {
			chaindata = path.Join(datadir, "tg", "chaindata")
		}
		//if snapshotDir == "" {
		//	snapshotDir = path.Join(datadir, "tg", "snapshot")
		//}
	},
	PersistentPostRun: func(cmd *cobra.Command, args []string) {
		debug.Exit()
	},
}

func Execute() {
	if err := rootCmd.ExecuteContext(rootContext()); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func withDatadir(cmd *cobra.Command) {
	cmd.Flags().StringVar(&datadir, "datadir", paths.DefaultDataDir(), "data directory for temporary ELT files")
	must(cmd.MarkFlagDirname("datadir"))
	cmd.Flags().StringVar(&mapSizeStr, "lmdb.mapSize", "", "map size for LMDB")

	cmd.Flags().StringVar(&chaindata, "chaindata", "", "path to the db")
	must(cmd.MarkFlagDirname("chaindata"))

	cmd.Flags().StringVar(&snapshotMode, "snapshot.mode", "", "set of snapshots to use")
	cmd.Flags().StringVar(&snapshotDir, "snapshot.dir", "", "snapshot dir")
	must(cmd.MarkFlagDirname("snapshot.dir"))

	cmd.Flags().StringVar(&database, "database", "", "lmdb|mdbx")
}

func openDatabase(path string) *ethdb.ObjectDatabase {
	db := ethdb.NewObjectDatabase(openKV(path, false))
	return db
}

func openKV(path string, exclusive bool) ethdb.RwKV {
	if database == "mdbx" {
		opts := ethdb.NewMDBX().Path(path)
		if exclusive {
			opts = opts.Exclusive()
		}
		if mapSizeStr != "" {
			var mapSize datasize.ByteSize
			must(mapSize.UnmarshalText([]byte(mapSizeStr)))
			opts = opts.MapSize(mapSize)
		}
		return opts.MustOpen()
	}

	opts := ethdb.NewLMDB().Path(path)
	if exclusive {
		opts = opts.Exclusive()
	}
	if mapSizeStr != "" {
		var mapSize datasize.ByteSize
		must(mapSize.UnmarshalText([]byte(mapSizeStr)))
		opts = opts.MapSize(mapSize)
	}
	return opts.MustOpen()
}