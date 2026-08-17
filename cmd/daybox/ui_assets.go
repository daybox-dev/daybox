package main

import _ "embed"

// ui_assets.go — embed the control-plane UI dashboard so the single daybox
// binary stays single (daybox's whole pitch). //go:embed can't reach above
// the package dir, so assets live under cmd/daybox/ui/, not repo-root
// web/ui/ (the existing web/ logos stay where they are).

//go:embed ui/index.html
var uiIndexHTML []byte
