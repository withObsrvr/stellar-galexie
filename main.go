package main

import (
	"fmt"
	"os"

	galexie "github.com/withObsrvr/stellar-galexie/internal"
)

func main() {
	err := galexie.Execute()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
