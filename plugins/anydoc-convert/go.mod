module github.com/suchi-dms/suchi/plugins/anydoc-convert

go 1.25.0

replace (
	github.com/suchi-dms/suchi/core => ../../core
	github.com/suchi-dms/suchi/plugin-api => ../../plugin-api
)

require (
	github.com/suchi-dms/suchi/core v0.0.0-00010101000000-000000000000
	github.com/suchi-dms/suchi/plugin-api v0.0.0-00010101000000-000000000000
)
