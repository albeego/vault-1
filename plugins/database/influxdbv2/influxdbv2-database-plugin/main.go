package main

import (
	"log"
	"os"

	"github.com/hashicorp/vault/plugins/database/influxdbv2"
	"github.com/hashicorp/vault/sdk/database/dbplugin/v5"
)

func main() {
	err := Run()
	if err != nil {
		log.Println(err)
		os.Exit(1)
	}
}

// Run instantiates a Influxdb object, and runs the RPC server for the plugin
func Run() error {
	dbType, err := influxdbv2.New()
	if err != nil {
		return err
	}

	dbplugin.Serve(dbType.(dbplugin.Database))

	return nil
}
