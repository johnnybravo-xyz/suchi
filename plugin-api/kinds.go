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
