#!/usr/bin/env bash
# Builds a throwaway module that imports the package from the source tree, the way a consumer does.
# Usage: scripts/consumer-check.sh <module root>
set -euo pipefail

src="$(realpath "${1:?module root}")"
dir="$(mktemp -d)"
cd "$dir"
cat > go.mod <<MOD
module example.test/consumer

go 1.26.9

require github.com/Hushscript/hushscript-go v0.0.0

replace github.com/Hushscript/hushscript-go => $src
MOD
cp "$src/go.sum" go.sum
cat > main.go <<'GO'
package main

import (
	"fmt"

	hushscript "github.com/Hushscript/hushscript-go"
)

func main() {
	c, err := hushscript.New(hushscript.WithToken("hsr1:eu:pat.00.00"))
	if err != nil {
		panic(err)
	}
	fmt.Println(c.API() != nil)
}
GO
GOFLAGS=-mod=mod go build -o /dev/null .
echo "consumer ok"
