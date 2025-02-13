// timescaledb-parallel-copy loads data from CSV format into a TimescaleDB database
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v4/stdlib"

	"github.com/timescale/timescaledb-parallel-copy/internal/batch"
	"github.com/timescale/timescaledb-parallel-copy/internal/db"
)

const (
	binName    = "timescaledb-parallel-copy"
	version    = "0.4.0"
	tabCharStr = "\\t"
)

// Flag vars
var (
	postgresConnect string
	overrides       []db.Overrideable
	schemaName      string
	tableName       string
	truncate        bool

	copyOptions    string
	splitCharacter string
	fromFile       string
	columns        string
	skipHeader     bool
	headerLinesCnt int

	workers         int
	limit           int64
	batchSize       int
	logBatches      bool
	reportingPeriod time.Duration
	verbose         bool
	showVersion     bool

	rowCount int64
)

// Parse args
func init() {
	var dbName string
	flag.StringVar(&postgresConnect, "connection", "host=localhost user=postgres sslmode=disable", "PostgreSQL connection url")
	flag.StringVar(&dbName, "db-name", "", "Database where the destination table exists")
	flag.StringVar(&tableName, "table", "test_table", "Destination table for insertions")
	flag.StringVar(&schemaName, "schema", "public", "Destination table's schema")
	flag.BoolVar(&truncate, "truncate", false, "Truncate the destination table before insert")

	flag.StringVar(&copyOptions, "copy-options", "CSV", "Additional options to pass to COPY (e.g., NULL 'NULL')")
	flag.StringVar(&splitCharacter, "split", ",", "Character to split by")
	flag.StringVar(&fromFile, "file", "", "File to read from rather than stdin")
	flag.StringVar(&columns, "columns", "", "Comma-separated columns present in CSV")
	flag.BoolVar(&skipHeader, "skip-header", false, "Skip the first line of the input")
	flag.IntVar(&headerLinesCnt, "header-line-count", 1, "Number of header lines")

	flag.IntVar(&batchSize, "batch-size", 5000, "Number of rows per insert")
	flag.Int64Var(&limit, "limit", 0, "Number of rows to insert overall; 0 means to insert all")
	flag.IntVar(&workers, "workers", 1, "Number of parallel requests to make")
	flag.BoolVar(&logBatches, "log-batches", false, "Whether to time individual batches.")
	flag.DurationVar(&reportingPeriod, "reporting-period", 0*time.Second, "Period to report insert stats; if 0s, intermediate results will not be reported")
	flag.BoolVar(&verbose, "verbose", false, "Print more information about copying statistics")

	flag.BoolVar(&showVersion, "version", false, "Show the version of this tool")

	flag.Parse()

	if dbName != "" {
		overrides = append(overrides, db.OverrideDBName(dbName))
	}
}

func getFullTableName() string {
	return fmt.Sprintf(`"%s"."%s"`, schemaName, tableName)
}

func main() {
	if showVersion {
		fmt.Printf("%s %s (%s %s)\n", binName, version, runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}

	if truncate { // Remove existing data from the table
		dbx, err := db.Connect(postgresConnect, overrides...)
		if err != nil {
			panic(err)
		}
		_, err = dbx.Exec(fmt.Sprintf("TRUNCATE %s", getFullTableName()))
		if err != nil {
			panic(err)
		}

		err = dbx.Close()
		if err != nil {
			panic(err)
		}
	}

	var reader io.Reader
	if len(fromFile) > 0 {
		file, err := os.Open(fromFile)
		if err != nil {
			log.Fatal(err)
		}
		defer file.Close()

		reader = file
	} else {
		reader = os.Stdin
	}

	if headerLinesCnt <= 0 {
		fmt.Printf("WARNING: provided --header-line-count (%d) must be greater than 0\n", headerLinesCnt)
		os.Exit(1)
	}

	var skip int
	if skipHeader {
		skip = headerLinesCnt

		if verbose {
			fmt.Printf("Skipping the first %d lines of the input.\n", headerLinesCnt)
		}
	}

	var wg sync.WaitGroup
	batchChan := make(chan net.Buffers, workers*2)

	// Generate COPY workers
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go processBatches(&wg, batchChan)
	}

	// Reporting thread
	if reportingPeriod > (0 * time.Second) {
		go report()
	}

	start := time.Now()
	if err := batch.Scan(batchSize, skip, limit, reader, batchChan); err != nil {
		log.Fatalf("Error reading input: %s", err.Error())
	}

	close(batchChan)
	wg.Wait()
	end := time.Now()
	took := end.Sub(start)

	rowsRead := atomic.LoadInt64(&rowCount)
	rowRate := float64(rowsRead) / float64(took.Seconds())

	res := fmt.Sprintf("COPY %d", rowsRead)
	if verbose {
		res += fmt.Sprintf(", took %v with %d worker(s) (mean rate %f/sec)", took, workers, rowRate)
	}
	fmt.Println(res)
}

// report periodically prints the write rate in number of rows per second
func report() {
	start := time.Now()
	prevTime := start
	prevRowCount := int64(0)

	for now := range time.NewTicker(reportingPeriod).C {
		rCount := atomic.LoadInt64(&rowCount)

		took := now.Sub(prevTime)
		rowrate := float64(rCount-prevRowCount) / float64(took.Seconds())
		overallRowrate := float64(rCount) / float64(now.Sub(start).Seconds())
		totalTook := now.Sub(start)

		fmt.Printf("at %v, row rate %0.2f/sec (period), row rate %0.2f/sec (overall), %E total rows\n", totalTook-(totalTook%time.Second), rowrate, overallRowrate, float64(rCount))

		prevRowCount = rCount
		prevTime = now
	}

}

// processBatches reads batches from channel c and copies them to the target
// server while tracking stats on the write.
func processBatches(wg *sync.WaitGroup, c chan net.Buffers) {
	dbx, err := db.Connect(postgresConnect, overrides...)
	if err != nil {
		panic(err)
	}
	defer dbx.Close()

	delimStr := "'" + splitCharacter + "'"
	if splitCharacter == tabCharStr {
		delimStr = "E" + delimStr
	}

	var copyCmd string
	if columns != "" {
		copyCmd = fmt.Sprintf("COPY %s(%s) FROM STDIN WITH DELIMITER %s %s", getFullTableName(), columns, delimStr, copyOptions)
	} else {
		copyCmd = fmt.Sprintf("COPY %s FROM STDIN WITH DELIMITER %s %s", getFullTableName(), delimStr, copyOptions)
	}

	for batch := range c {
		start := time.Now()
		rows, err := db.CopyFromLines(dbx, &batch, copyCmd)
		if err != nil {
			panic(err)
		}
		atomic.AddInt64(&rowCount, rows)

		if logBatches {
			took := time.Since(start)
			fmt.Printf("[BATCH] took %v, batch size %d, row rate %f/sec\n", took, batchSize, float64(batchSize)/float64(took.Seconds()))
		}
	}
	wg.Done()
}
