package mcpserver

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
)

func TestOKResultRejectsOversizedPublicResponse(t *testing.T) {
	deps := Deps{maxResponseBytes: 128}
	res, _, err := okResult(deps, restic.CostLight, map[string]string{
		"value": strings.Repeat("a", 512),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.IsError {
		t.Fatal("expected a response-size error result")
	}
	var text strings.Builder
	for _, content := range res.Content {
		if tc, ok := content.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if !strings.Contains(text.String(), "response_limit_exceeded") {
		t.Fatalf("missing response_limit_exceeded: %s", text.String())
	}
}
