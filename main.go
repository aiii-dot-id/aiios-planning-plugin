package main

import sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"

const pluginID = "id.aeon.planning"

func newPlanningPlugin() *sdk.Plugin {
	p := sdk.New(pluginID)
	registerReview(p)
	return p
}

func currentWorkRequirement(reason string) string {
	if reason == "session_lookup_failed" {
		return "the host could not look up the acting work session; resolve that lookup failure before retrying; do not invent a session ID or start replacement work"
	}
	return "no acting work session: pass work_session_id, which work start and work deliver print (ws_...)"
}
