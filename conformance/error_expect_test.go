package conformance

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	hushscript "github.com/Hushscript/hushscript-go"
)

func TestErrorExpectAssertsExplicitNullNumbersAsAbsent(t *testing.T) {
	for _, field := range []string{"status", "numericCode", "retryAfter"} {
		for _, tc := range []struct {
			name    string
			json    string
			value   int
			problem bool
		}{
			{"omitted", "", 1, false},
			{"null absent", "null", 0, false},
			{"null present", "null", 1, true},
			{"number equal", "1", 1, false},
			{"number different", "1", 0, true},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				body := `{"class":"NetworkError","code":"network_error"}`
				if tc.json != "" {
					body = fmt.Sprintf(`{"class":"NetworkError","code":"network_error",%q:%s}`, field, tc.json)
				}
				var want errorExpect
				if err := json.Unmarshal([]byte(body), &want); err != nil {
					t.Fatal(err)
				}
				e := &hushscript.Error{Kind: hushscript.KindNetwork, Code: "network_error"}
				switch field {
				case "status":
					e.Status = tc.value
				case "numericCode":
					e.NumericCode = tc.value
				case "retryAfter":
					e.RetryAfter = time.Duration(tc.value) * time.Second
				}
				got := checkError(e, &want)
				if tc.problem {
					if len(got) != 1 || !strings.HasPrefix(got[0], field+" ") {
						t.Fatalf("problems = %v, want exactly one %s mismatch", got, field)
					}
				} else if len(got) != 0 {
					t.Fatalf("unexpected problems: %v", got)
				}
			})
		}
	}
}
