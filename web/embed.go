// Package web holds the dashboard: plain HTML, CSS and JavaScript with no
// build step and nothing loaded from outside, embedded into fleetd.
package web

import "embed"

//go:embed index.html app.js style.css
var FS embed.FS
