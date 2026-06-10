// dialectbench recreates the phase-B harness from esilver/duckdb-go-pure#1:
// per-row prepared INSERT exec inside one transaction, through the
// googlesqlite dialect layer, plus an R phase that runs the same shape
// through the raw engine driver for the floor.
package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	convergeduckdb "github.com/esilver/duckdb-go-pure"
	_ "github.com/goccy/googlesqlite"
)

var rows = 3000

func phase(name string, db *sql.DB, useTx bool, n int, insertSQL string) {
	start := time.Now()
	var execer interface {
		Prepare(query string) (*sql.Stmt, error)
	} = db
	var tx *sql.Tx
	var err error
	if useTx {
		tx, err = db.Begin()
		if err != nil {
			fmt.Printf("%-22s BEGIN err: %v\n", name, err)
			return
		}
		execer = tx
	}
	stmt, err := execer.Prepare(insertSQL)
	if err != nil {
		fmt.Printf("%-22s prepare err: %v\n", name, err)
		return
	}
	for i := 0; i < n; i++ {
		if _, err := stmt.Exec(int64(i), int64(i%1000), float64(i)/7.0); err != nil {
			fmt.Printf("%-22s exec err at row %d: %v\n", name, i, err)
			return
		}
	}
	stmt.Close()
	if tx != nil {
		if err := tx.Commit(); err != nil {
			fmt.Printf("%-22s COMMIT err: %v\n", name, err)
			return
		}
	}
	el := time.Since(start)
	fmt.Printf("%-22s %5d rows in %8.0f ms  -> %6.3f ms/row\n", name, n, float64(el.Milliseconds()), float64(el.Microseconds())/1000.0/float64(n))
}

func main() {
	if v := os.Getenv("ROWS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rows = n
		}
	}
	sel := os.Getenv("PHASES")
	if sel == "" {
		sel = "B"
	}
	on := map[string]bool{}
	for _, p := range strings.Split(sel, ",") {
		on[p] = true
	}

	fpath := "/tmp/dialectbench_file.db"
	os.Remove(fpath)
	os.Remove(fpath + ".wal")
	fileDB, err := sql.Open("googlesqlite", fmt.Sprintf("file:%s?cache=shared", fpath))
	if err != nil {
		panic(err)
	}
	if _, err := fileDB.Exec("CREATE TABLE `t` (`id` INT64, `grp` INT64, `val` FLOAT64)"); err != nil {
		fmt.Printf("create table err: %v\n", err)
		os.Exit(1)
	}

	// raw engine floor: same DB file family, separate file, via the
	// internal duckdb driver registered by the googlesqlite package.
	rpath := "/tmp/dialectbench_raw.db"
	var rawDB *sql.DB
	if on["R"] {
		os.Remove(rpath)
		os.Remove(rpath + ".wal")
		rawDB, err = sql.Open("googlesqlite_duckdb", rpath)
		if err != nil {
			panic(err)
		}
		if _, err := rawDB.Exec("CREATE TABLE t (id BIGINT, grp BIGINT, val DOUBLE)"); err != nil {
			fmt.Printf("raw create table err: %v\n", err)
			os.Exit(1)
		}
	}

	// U phase: raw engine driver, but with ~350 dummy scalar UDFs
	// registered first — isolates the catalog-size effect the dialect
	// layer's googlesqlite_* registrations impose on per-exec cost.
	upath := "/tmp/dialectbench_udf.db"
	var udfDB *sql.DB
	if on["U"] {
		os.Remove(upath)
		os.Remove(upath + ".wal")
		udfDB, err = sql.Open("googlesqlite_duckdb", upath)
		if err != nil {
			panic(err)
		}
		conn, err := udfDB.Conn(context.Background())
		if err != nil {
			panic(err)
		}
		const varcharType = 17 // DUCKDB_TYPE_VARCHAR
		for i := 0; i < 350; i++ {
			name := fmt.Sprintf("dummy_udf_%d", i)
			if err := convergeduckdb.RegisterScalarConn(conn, name, []int32{varcharType}, varcharType, -1, false, false, func(args []driver.Value) (any, error) {
				return args[0], nil
			}); err != nil {
				fmt.Printf("udf register err at %d: %v\n", i, err)
				os.Exit(1)
			}
		}
		conn.Close()
		if _, err := udfDB.Exec("CREATE TABLE t (id BIGINT, grp BIGINT, val DOUBLE)"); err != nil {
			fmt.Printf("udf create table err: %v\n", err)
			os.Exit(1)
		}
	}

	// warmup outside the profile window
	phase("warmup(file,tx)", fileDB, true, 50, "INSERT `t` (`id`,`grp`,`val`) VALUES (?,?,?)")

	cpuPath := os.Getenv("CPUPROF")
	if cpuPath == "" {
		cpuPath = "/tmp/dialectbench.cpu"
	}
	prof, _ := os.Create(cpuPath)
	pprof.StartCPUProfile(prof)

	if on["B"] {
		phase("B file,one-tx,dialect", fileDB, true, rows, "INSERT `t` (`id`,`grp`,`val`) VALUES (?,?,?)")
	}
	if on["R"] {
		phase("R file,one-tx,raw", rawDB, true, rows, "INSERT INTO t (id, grp, val) VALUES (?,?,?)")
	}
	if on["U"] {
		phase("U file,one-tx,raw+udfs", udfDB, true, rows, "INSERT INTO t (id, grp, val) VALUES (?,?,?)")
	}

	pprof.StopCPUProfile()
	prof.Close()

	memPath := os.Getenv("MEMPROF")
	if memPath == "" {
		memPath = "/tmp/dialectbench.mem"
	}
	mf, _ := os.Create(memPath)
	runtime.GC()
	pprof.Lookup("allocs").WriteTo(mf, 0)
	mf.Close()

	var n int64
	if err := fileDB.QueryRow("SELECT COUNT(*) FROM `t`").Scan(&n); err != nil {
		fmt.Printf("count err: %v\n", err)
	}
	fmt.Printf("dialect file table rows: %d\n", n)
	fileDB.Close()
	if rawDB != nil {
		if err := rawDB.QueryRow("SELECT COUNT(*) FROM t").Scan(&n); err != nil {
			fmt.Printf("raw count err: %v\n", err)
		}
		fmt.Printf("raw file table rows: %d\n", n)
		rawDB.Close()
	}
	if udfDB != nil {
		if err := udfDB.QueryRow("SELECT COUNT(*) FROM t").Scan(&n); err != nil {
			fmt.Printf("udf count err: %v\n", err)
		}
		fmt.Printf("udf file table rows: %d\n", n)
		udfDB.Close()
	}
}
