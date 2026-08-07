package lang

import "strings"

// FromPDFLang normalises a PDF /Lang tag (a BCP-47 / RFC 3066
// language tag) to a 2-letter ISO-639-1 code. PDF authors write
// values like "en", "en-US", "de-DE", "zh-Hant" — we take the
// primary subtag and lowercase.
//
//	FromPDFLang("en")       == "en"
//	FromPDFLang("en-US")    == "en"
//	FromPDFLang("DE-de")    == "de"
//	FromPDFLang("zh-Hant")  == "zh"
//	FromPDFLang("")         == ""
//	FromPDFLang("xxx-YY")   == ""    // not ISO-639-1 shape
func FromPDFLang(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return ""
	}
	primary := strings.SplitN(tag, "-", 2)[0]
	primary = strings.ToLower(strings.TrimSpace(primary))
	if !iso6391Re.MatchString(primary) {
		return ""
	}
	return primary
}

// FromContentLanguage normalises an RFC 3282 Content-Language
// header, which is a comma-separated list of BCP-47 tags. We
// take the first tag's primary subtag; the header form
// `de, en;q=0.5` is common on multilingual email.
//
//	FromContentLanguage("de")             == "de"
//	FromContentLanguage("de, en;q=0.5")   == "de"
//	FromContentLanguage("")               == ""
func FromContentLanguage(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	first := strings.SplitN(header, ",", 2)[0]
	// Strip q-value etc. — anything after ';'.
	first = strings.SplitN(first, ";", 2)[0]
	return FromPDFLang(first)
}
