// Package web holds the page, built into the program.
package web

import "embed"

// FS holds index.html, app.js and style.css.
//
//go:embed index.html app.js style.css
var FS embed.FS
