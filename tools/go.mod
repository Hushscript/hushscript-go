module github.com/Hushscript/hushscript-sdk-go/tools

go 1.25.0

tool (
	github.com/avito-tech/go-mutesting/cmd/go-mutesting
	github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen
	golang.org/x/exp/cmd/apidiff
	golang.org/x/vuln/cmd/govulncheck
	mvdan.cc/gofumpt
)

require (
	github.com/avast/retry-go v3.0.0+incompatible // indirect
	// Newer go-mutesting requires Go 1.25.5 or later; preserve the 1.25.0 floor.
	github.com/avito-tech/go-mutesting v0.0.0-20250418092011-3ce278f4e19f // indirect
	// Newer go-yit changes YAML node types, breaking vmware-labs/yaml-jsonpath.
	github.com/dprotaso/go-yit v0.0.0-20250513224043-18a80f8f6df4 // indirect
	github.com/fatih/color v1.19.0 // indirect
	github.com/getkin/kin-openapi v0.151.0 // indirect
	github.com/go-openapi/jsonpointer v1.0.0 // indirect
	github.com/go-openapi/testify/v2 v2.7.0 // indirect
	github.com/jessevdk/go-flags v1.6.1 // indirect
	github.com/mattn/go-colorable v0.1.16 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mitchellh/colorstring v0.0.0-20190213212951-d06e56a500db // indirect
	github.com/oapi-codegen/oapi-codegen/v2 v2.8.0 // indirect
	github.com/oasdiff/yaml v0.1.1 // indirect
	github.com/oasdiff/yaml3 v0.0.14 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	github.com/schollz/progressbar/v3 v3.19.1 // indirect
	github.com/sergi/go-diff v1.4.0 // indirect
	github.com/speakeasy-api/jsonpath v0.6.3 // indirect
	github.com/speakeasy-api/openapi v1.25.1 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	github.com/termie/go-shutil v0.0.0-20140729215957-bcacb06fecae // indirect
	github.com/ulikunitz/xz v0.5.17 // indirect
	github.com/vmware-labs/yaml-jsonpath v0.3.2 // indirect
	github.com/zimmski/osutil v1.7.1 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/exp v0.0.0-20260820122028-d6e0b57b1a69 // indirect
	golang.org/x/mod v0.40.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/telemetry v0.0.0-20260819135818-0a2e198f32ee // indirect
	golang.org/x/term v0.45.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.org/x/tools v0.49.0 // indirect
	golang.org/x/vuln v1.7.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	mvdan.cc/gofumpt v0.11.0 // indirect
)
