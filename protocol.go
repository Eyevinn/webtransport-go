package webtransport

// settingsEnableWebtransportDraft06 is the value for ENABLE_WEBTRANSPORT
// that was used up until draft-ietf-webtrans-http3-06.
const settingsEnableWebtransportDraft06 = 0x2b603742

// settingsWebTransportMaxSessions is SETTINGS_WEBTRANSPORT_MAX_SESSIONS,
// used in drafts 07 through 14. A non-zero value advertises WebTransport
// support and the maximum number of concurrent sessions the server will
// accept. Several public deployments (Cloudflare's MoQ interop relay,
// moq-rs / cdn.moq.dev, …) still emit only this setting.
const settingsWebTransportMaxSessions = 0xc671706a

// settingsWebTransportEnabled is the value for SETTINGS_WT_ENABLED, introduced
// in draft-ietf-webtrans-http3-15. A non-zero value advertises WebTransport
// support.
const settingsWebTransportEnabled = 0x2c7cf000

const protocolHeader = "webtransport"
