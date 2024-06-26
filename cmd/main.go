package main

import (
	"fmt"
	"os"

	"github.com/hkust-adsl/kubernetes-scheduler-simulator/cmd/simon"
)

func main() {
	// In the simon package we essentially instantiate the command line interface with the functionalities provided by the cobra library.
	cmd := simon.NewSimonCommand()
	
	if err := cmd.Execute(); err != nil {
		fmt.Printf("start with error: %s", err.Error())
		os.Exit(1)
	}
}
