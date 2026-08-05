module github.com/johnnybravo-xyz/suchi/plugins/anydoc-convert

go 1.25.0

replace (
	github.com/johnnybravo-xyz/suchi/core => ../../core
	github.com/johnnybravo-xyz/suchi/plugin-api => ../../plugin-api
)

require (
	github.com/johnnybravo-xyz/suchi/core v0.0.0-00010101000000-000000000000
	github.com/johnnybravo-xyz/suchi/plugin-api v0.0.0-00010101000000-000000000000
)
