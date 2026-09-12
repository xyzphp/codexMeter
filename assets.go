package main

import _ "embed"

// The two embedded pages intentionally have different layouts: indexHTML is
// the compact LX04 WebView page, while browserHTML is the browser workbench.
// There is no CDN or separate frontend build step.
//
//go:embed web/index.html
var indexHTML []byte

//go:embed web/browser.html
var browserHTML []byte

//go:embed web/settings.html
var settingsHTML []byte

//go:embed web/api-docs.html
var apiDocsHTML []byte

//go:embed web/setup.html
var setupHTML []byte

//go:embed openapi.yaml
var openAPISpec []byte

//go:embed web/assets/account-credentials-guide.png
var accountCredentialsGuide []byte
