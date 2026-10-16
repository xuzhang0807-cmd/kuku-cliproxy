// logo.go holds the provider icon the plugin publishes to the management UI.
//
// The host resolves a provider's icon from a table for the providers it ships
// with and falls back to a generic plug glyph for anything else. The one
// channel a plugin controls is the `logo` field of its registration metadata,
// which the panel reads for the plugin list, its OAuth card and the sidebar.
//
// The icon is inlined as a base64 data URL rather than referenced over HTTP:
// the panel is rendered in the operator's browser, which may not be able to
// reach the same hosts the server can, and the host passes this value through
// html.EscapeString, which mangles the `<` of a raw inline SVG but leaves
// base64 (A-Za-z0-9+/=) untouched.
//
// Source: the official 库库AI mark that kuku.baidu.com itself declares as
// <link rel="icon" sizes="48x48">. Regenerate with:
//
//	curl -sL https://staticsns.cdn.bcebos.com/amis/2026-8/1786638690164/logo.png | base64 -w0
//
// A 48x48 PNG, 1729 bytes.

package main

// kukuLogoPNGBase64 is the official 48x48 logo, base64-encoded.
const kukuLogoPNGBase64 = "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" +
	"gg=="

// kukuLogoDataURL is what the host actually sees in the registration metadata.
var kukuLogoDataURL = "data:image/png;base64," + kukuLogoPNGBase64
