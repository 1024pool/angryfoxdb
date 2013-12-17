package main

import (
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"

	"github.com/angryfoxsu/levigo"
	conf "github.com/angryfoxsu/goconfig"
)

var DB *levigo.DB
var DefaultReadOptions   = levigo.NewReadOptions()
var DefaultWriteOptions  = levigo.NewWriteOptions()
var ReadWithoutCacheFill = levigo.NewReadOptions()

func openDB(level_data string) {
	opts := levigo.NewOptions()
	cache := levigo.NewLRUCache(128 * 1024 * 1024) // 128MB cache
	opts.SetCache(cache)
	filter := levigo.NewBloomFilter(10)
	opts.SetFilterPolicy(filter)
	opts.SetCreateIfMissing(true)

	var err error
	DB, err = levigo.Open(level_data, opts)
	maybeFatal(err)
}

func maybeFatal(err error) {
	if err != nil {
		fmt.Printf("Fatal error: %s\n", err)
		os.Exit(1)
	}
}

func main() {
	c, err := conf.LoadConfigFile("conf/default.ini")
	if err != nil {
		maybeFatal(err)
	}

	// GetValue
	value, _ := c.GetValue("DEFAULT", "flag") // return "Let's use GoConfig!!!"
	if value != "1" {
		log.Printf("\nExpect: %s\nMissing: %s\n", "config!!!", value)
	}
	server, _     := c.GetValue("DEFAULT", "server")
	port, _       := c.GetValue("DEFAULT", "port")
	level_data, _ := c.GetValue("DEFAULT", "level_data")
	if len(server) < 3 || len(port) < 3 || len(level_data) < 3 {
		log.Printf("\nExpect: %s\nServer MissConfig Port MissConfig: %s\n", server , port)
	}
	
	runtime.GOMAXPROCS(runtime.NumCPU()-1)
	openDB(level_data)
	
	go func() {
		log.Println(http.ListenAndServe(server + ":" + port, nil))
	}()
	listen(port)
}

func init() {
	ReadWithoutCacheFill.SetFillCache(false)
}
