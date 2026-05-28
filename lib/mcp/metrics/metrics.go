package metrics

import (
	"github.com/cosmos/cosmos-sdk/telemetry"
	gometrics "github.com/hashicorp/go-metrics"

	libmetrics "github.com/dydxprotocol/v4-chain/protocol/lib/metrics"
)

// ServiceName prefixes every metric the MCP server emits so they aggregate
// cleanly in dashboards alongside the rest of the protocol's telemetry.
const ServiceName = "mcp_server"

// IncToolCall bumps the per-tool call counter labelled by tenant.
func IncToolCall(tool, tenantID string) {
	telemetry.IncrCounterWithLabels(
		[]string{ServiceName, "tool_call_count"},
		1,
		[]gometrics.Label{
			libmetrics.GetLabelForStringValue("tool", tool),
			libmetrics.GetLabelForStringValue("tenant", tenantID),
		},
	)
}

// IncToolError bumps the per-tool error counter labelled by reason so
// policy denials, rate-limits, chain rejects, etc. are distinguishable.
func IncToolError(tool, tenantID, reason string) {
	telemetry.IncrCounterWithLabels(
		[]string{ServiceName, "tool_error_count"},
		1,
		[]gometrics.Label{
			libmetrics.GetLabelForStringValue("tool", tool),
			libmetrics.GetLabelForStringValue("tenant", tenantID),
			libmetrics.GetLabelForStringValue("reason", reason),
		},
	)
}

// IncBroadcastResult bumps the broadcast-outcome counter (accept / chain-reject
// / network-error) labelled by tenant.
func IncBroadcastResult(tenantID, outcome string) {
	telemetry.IncrCounterWithLabels(
		[]string{ServiceName, "broadcast_count"},
		1,
		[]gometrics.Label{
			libmetrics.GetLabelForStringValue("tenant", tenantID),
			libmetrics.GetLabelForStringValue("outcome", outcome),
		},
	)
}
