package commands

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/activecm/rita/parser"
	"github.com/activecm/rita/resources"
	"github.com/activecm/rita/util"
	"github.com/urfave/cli"
)

func init() {
	importCommand := cli.Command{
		Name:  "import",
		Usage: "Import bro logs into a target database",
		UsageText: "rita import [command options] [<import directory> <database root>]\n\n" +
			"Logs directly in <import directory> will be imported into a database" +
			" named <database root>. Files in a subfolder of <import directory> will be imported" +
			" into <database root>-$SUBFOLDER_NAME. <import directory>" +
			" and <database root> will be loaded from the configuration file unless" +
			" BOTH arguments are supplied.",
		Flags: []cli.Flag{
			threadFlag,
			configFlag,
			rollingFlag,
			totalChunksFlag,
			currentChunkFlag,
		},
		Action: func(c *cli.Context) error {
			importer := NewImporter(c)
			err := importer.run()
			fmt.Printf(updateCheck(c.String("config")))
			return err
		},
	}

	bootstrapCommands(importCommand)
}

type (
	//Importer ...
	Importer struct {
		res            *resources.Resources
		configFile     string
		importDir      string
		targetDatabase string
		rolling        bool
		totalChunks    int
		currentChunk   int
		threads        int
	}
)

//NewImporter ....
func NewImporter(c *cli.Context) *Importer {
	return &Importer{
		configFile:     c.String("config"),
		importDir:      c.Args().Get(0),
		targetDatabase: c.Args().Get(1),
		rolling:        c.Bool("rolling"),
		totalChunks:    c.Int("numchunks"),
		currentChunk:   c.Int("chunk"),
		threads:        util.Max(c.Int("threads")/2, 1),
	}
}

func (i *Importer) parseArgs() error {

	//check if one argument is set but not the other
	if i.importDir == "" || i.targetDatabase == "" {
		return cli.NewExitError("\n\t[!] Both <directory to import> and <database prefix> are required.", -1)
	}

	// check if rolling flag was passed in
	if i.rolling {

		// verify that required flag values were provided
		if (i.totalChunks == -1) || (i.currentChunk == -1) {
			return cli.NewExitError("\n\t[!] Both `--numchunks <total number of chunks>` and `--chunk <current chunk number>` must be provided for rolling analysis import.", -1)
		}

		// verifies the chunk is a divisor of 24 (we currently support 24 hour's worth of data in a rolling dataset)
		if !(i.totalChunks > 0) || ((24 % i.totalChunks) != 0) {
			return cli.NewExitError("\n\t[!] Total number of chunks must be a divisor of 24 (Valid chunk sizes: 1, 2, 4, 6, 8, 12)", -1)
		}

		// validate chunk size
		if !(i.currentChunk > 0) {
			return cli.NewExitError("\n\t[!] Current chunk number must be greater than 0", -1)
		}

		if i.currentChunk > i.totalChunks {
			return cli.NewExitError("\n\t[!] Current chunk number cannot be greater than the total number of chunks", -1)
		}

	}

	i.res = resources.InitResources(i.configFile)

	return nil
}

func (i *Importer) setTargetDatabase() error {
	// get all database names
	names, _ := i.res.DB.Session.DatabaseNames()

	// check if database exists
	dbExists := util.StringInSlice(i.targetDatabase, names)

	// Add new metadatabase record for db if doesn't already exist
	if !dbExists {
		err := i.res.MetaDB.AddNewDB(i.targetDatabase)
		if err != nil {
			return cli.NewExitError(fmt.Errorf("\n\t[!] %v", err.Error()), -1)
		}
	}

	i.res.DB.SelectDB(i.targetDatabase)
	i.res.Config.S.Bro.DBRoot = i.targetDatabase

	return nil
}

func (i *Importer) setImportDirectory() error {

	// parse directory path
	filePath, err := filepath.Abs(i.importDir)
	if err != nil {
		return cli.NewExitError(err.Error(), -1)
	}

	// check if directory exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return cli.NewExitError(fmt.Errorf("\n\t[!] %v", err.Error()), -1)
	}

	// assign import directory
	i.res.Config.S.Bro.ImportDirectory = i.importDir
	return nil
}

func (i *Importer) setRolling() error {
	if i.rolling {
		// verify that numchunks matches originally set value if database was already
		// set as a rolling database in previous imports
		err := i.res.MetaDB.VerifyIfAlreadyRollingDB(i.targetDatabase, i.totalChunks, i.currentChunk)
		if err != nil {
			return cli.NewExitError(fmt.Errorf("\n\t[!] %v", err.Error()), -1)
		}

		// set stuff if no errors
		i.res.Config.S.Bro.Rolling = true
		i.res.Config.S.Bro.TotalChunks = i.totalChunks
		i.res.Config.S.Bro.CurrentChunk = i.currentChunk - 1
	} else {
		// set single import defaults (1 total chunks, and we're on the first and only chunk)
		i.res.Config.S.Bro.Rolling = false
		i.res.Config.S.Bro.TotalChunks = 1
		i.res.Config.S.Bro.CurrentChunk = 0
	}

	return nil
}

// run runs the importer
func (i *Importer) run() error {
	// verify command line arguments
	err := i.parseArgs()
	if err != nil {
		return err
	}

	// set up import directory
	err = i.setImportDirectory()
	if err != nil {
		return err
	}

	// set up target database
	err = i.setTargetDatabase()
	if err != nil {
		return err
	}

	// set up rolling stats if they apply
	err = i.setRolling()
	if err != nil {
		return err
	}

	importer := parser.NewFSImporter(i.res, i.threads, i.threads)
	if len(importer.GetInternalSubnets()) == 0 {
		return cli.NewExitError("Internal subnets are not defined. Please set the InternalSubnets section of the config file.", -1)
	}

	i.res.Log.Infof("Importing %s\n", i.res.Config.S.Bro.ImportDirectory)
	fmt.Println("\n\t[+] Importing " + i.res.Config.S.Bro.ImportDirectory + " :")

	datastore := parser.NewMongoDatastore(i.res.DB.Session, i.res.MetaDB,
		i.res.Config.S.Bro.ImportBuffer, i.res.Log)

	importer.Run(datastore)

	i.res.Log.Infof("Finished importing %s\n", i.res.Config.S.Bro.ImportDirectory)

	return nil
}
