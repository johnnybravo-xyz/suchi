package pluginapi

// Plugin kinds. Enum-of-strings on purpose: config files use these and the
// distro's plugins/index.go blank-imports one package per plugin, so we do
// not need type safety here — just a shared vocabulary.
const (
	KindIngest   = "ingest"
	KindSniff    = "sniff"
	KindOCR      = "ocr"
	KindStorage  = "storage"
	KindSearch   = "search"
	KindAuth     = "auth"
	KindClassify = "classify"
	KindNotify   = "notify"
	KindExport   = "export"
	KindConvert  = "convert"
	KindBarcode  = "barcode"
)

// BrandPrefix is the hardcoded prefix suchi puts in front of every
// operator-visible display identifier it emits to external systems
// (Entra OAuth app names today; webhook display names, third-party
// integration labels tomorrow). Composed as `BrandPrefix + <operator
// nickname>`. Lives here so any module in the tree — core or plugin —
// inherits the same convention without a fresh package.
const BrandPrefix = "Suchi - "
