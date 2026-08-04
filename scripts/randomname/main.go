package main

import (
	"fmt"
	"log"
	"os"

	"github.com/indexus/go-indexus-core/encoding"
)

func main() {
	preferNear := os.Getenv("INDEXUS_PREFER_NEAR")
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "--near" && i+1 < len(os.Args) {
			preferNear = os.Args[i+1]
			break
		}
	}
	var (
		n   string
		err error
	)
	if preferNear != "" {
		if target, decErr := encoding.BASE64.Decode(preferNear); decErr == nil {
			n, err = encoding.BASE64.RandomNameNear(target, 20)
		}
	}
	if n == "" {
		n, err = encoding.BASE64.RandomName()
	}
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(n)
}
