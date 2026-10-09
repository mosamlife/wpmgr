package agentcmd

// MinAgentVersionForBuilderFacts is the first agent release that reports the
// builder_facts object on its metadata push: the parent theme directory and
// whether Elementor's Atomic editor is on. An older agent sends neither, and
// the AI readiness checklist then says the plugin needs updating rather than
// guessing the answer is "off".
//
// This is a floor for DISPLAY only. Nothing is allowed or refused because of
// it. It names the release that carries the collector, so it moves to the
// number that release actually shipped as; the pin test beside it fails when
// the agent tree carries the collector but ships below this number.
const MinAgentVersionForBuilderFacts = "0.61.159"
