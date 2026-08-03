module github.com/johnnybravo-xyz/suchi/plugins/oidc

go 1.25.0

require (
	github.com/coreos/go-oidc/v3 v3.11.0
	github.com/johnnybravo-xyz/suchi/core v0.0.0-00010101000000-000000000000
	github.com/johnnybravo-xyz/suchi/plugin-api v0.0.0-00010101000000-000000000000
	golang.org/x/oauth2 v0.24.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-jose/go-jose/v4 v4.0.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/crypto v0.25.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	modernc.org/gc/v3 v3.0.0-20240107210532-573471604cb6 // indirect
	modernc.org/libc v1.55.3 // indirect
	modernc.org/mathutil v1.6.0 // indirect
	modernc.org/memory v1.8.0 // indirect
	modernc.org/sqlite v1.34.1 // indirect
	modernc.org/strutil v1.2.0 // indirect
	modernc.org/token v1.1.0 // indirect
)

replace (
	github.com/johnnybravo-xyz/suchi/core => ../../core
	github.com/johnnybravo-xyz/suchi/plugin-api => ../../plugin-api
)
