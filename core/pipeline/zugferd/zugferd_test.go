package zugferd

import (
	"strings"
	"testing"
)

// A trimmed-but-valid Factur-X BASIC-WL sample. Structure mirrors what
// a real ERP would emit; profile URN and namespaces are omitted since
// the parser matches by local name only.
const sampleCII = `<?xml version="1.0" encoding="UTF-8"?>
<CrossIndustryInvoice>
  <ExchangedDocument>
    <ID>INV-2026-0042</ID>
    <IssueDateTime>
      <DateTimeString format="102">20260731</DateTimeString>
    </IssueDateTime>
  </ExchangedDocument>
  <SupplyChainTradeTransaction>
    <ApplicableHeaderTradeAgreement>
      <SellerTradeParty>
        <Name>BESCOM Electricity Supply Co.</Name>
        <SpecifiedTaxRegistration>
          <ID schemeID="VA">DE123456789</ID>
        </SpecifiedTaxRegistration>
      </SellerTradeParty>
      <BuyerTradeParty>
        <Name>Alice Example</Name>
      </BuyerTradeParty>
    </ApplicableHeaderTradeAgreement>
    <ApplicableHeaderTradeSettlement>
      <InvoiceCurrencyCode>INR</InvoiceCurrencyCode>
      <SpecifiedTradeSettlementHeaderMonetarySummation>
        <LineTotalAmount>4200.00</LineTotalAmount>
        <GrandTotalAmount>4523.00</GrandTotalAmount>
      </SpecifiedTradeSettlementHeaderMonetarySummation>
    </ApplicableHeaderTradeSettlement>
  </SupplyChainTradeTransaction>
</CrossIndustryInvoice>`

func TestParseCII_HappyPath(t *testing.T) {
	inv, err := parseCII([]byte(sampleCII))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := Invoice{
		Number:     "INV-2026-0042",
		IssueDate:  "2026-07-31",
		Currency:   "INR",
		TotalGross: 4523.00,
		TotalNet:   4200.00,
		SellerName: "BESCOM Electricity Supply Co.",
		SellerVAT:  "DE123456789",
		BuyerName:  "Alice Example",
	}
	if inv.Number != want.Number ||
		inv.IssueDate != want.IssueDate ||
		inv.Currency != want.Currency ||
		inv.TotalGross != want.TotalGross ||
		inv.TotalNet != want.TotalNet ||
		inv.SellerName != want.SellerName ||
		inv.SellerVAT != want.SellerVAT ||
		inv.BuyerName != want.BuyerName {
		t.Fatalf("mismatch:\n got %+v\nwant %+v", inv, want)
	}
}

func TestParseCII_BOM(t *testing.T) {
	// Add UTF-8 BOM prefix — some emitters produce it and encoding/xml
	// rejects the input; trimBOM must strip it.
	bom := append([]byte{0xEF, 0xBB, 0xBF}, []byte(sampleCII)...)
	if _, err := parseCII(bom); err != nil {
		t.Fatalf("BOM parse: %v", err)
	}
}

func TestParseCII_Empty(t *testing.T) {
	if _, err := parseCII([]byte("")); err == nil {
		t.Fatal("want error on empty input")
	}
}

func TestParseCIIDate(t *testing.T) {
	cases := map[string]string{
		"20260731": "2026-07-31",
		"":         "",
		"2026-07":  "",
		"20261301": "2026-13-01", // parser is format-only, doesn't validate
		"abcdefgh": "",
	}
	for in, want := range cases {
		if got := parseCIIDate(in); got != want {
			t.Errorf("parseCIIDate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPickInvoiceAttachment(t *testing.T) {
	cases := []struct {
		list string
		want string
	}{
		{"factur-x.xml -> 42\nextra.txt -> 43", "factur-x.xml"},
		{"README.md -> 1\nZUGFeRD-invoice.xml -> 7", "ZUGFeRD-invoice.xml"},
		{"XRechnung.XML -> 9", "XRechnung.XML"},
		{"only-random.dat -> 3\nmisc.pdf -> 4", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := pickInvoiceAttachment(c.list); got != c.want {
			t.Errorf("pickInvoiceAttachment(%q) = %q, want %q", c.list, got, c.want)
		}
	}
}

func TestPickSellerVAT_FallbackToFirst(t *testing.T) {
	regs := []ciiTaxReg{
		{ID: ciiSchemeID{Value: "FC123", Scheme: "FC"}},
		{ID: ciiSchemeID{Value: "GST987", Scheme: "GST"}},
	}
	if got := pickSellerVAT(regs); got != "FC123" {
		t.Errorf("fallback: got %q want FC123", got)
	}
	// Prefer VA scheme when present.
	regs = append([]ciiTaxReg{{ID: ciiSchemeID{Value: "DE42", Scheme: "VA"}}}, regs...)
	if got := pickSellerVAT(regs); got != "DE42" {
		t.Errorf("prefer VA: got %q want DE42", got)
	}
}

func TestParseFloat(t *testing.T) {
	cases := map[string]float64{
		"1234.56": 1234.56,
		"0":       0,
		"":        0,
		"nope":    0,
	}
	for in, want := range cases {
		if got := parseFloat(in); got != want {
			t.Errorf("parseFloat(%q) = %v, want %v", in, got, want)
		}
	}
}

// Guard against regressions where the parser silently ignores a field.
func TestParseCII_NoSummary(t *testing.T) {
	shrunk := strings.Replace(sampleCII,
		"<SpecifiedTradeSettlementHeaderMonetarySummation>",
		"<SpecifiedTradeSettlementHeaderMonetarySummation><!-- empty -->",
		1)
	shrunk = strings.Replace(shrunk,
		"<LineTotalAmount>4200.00</LineTotalAmount>", "", 1)
	shrunk = strings.Replace(shrunk,
		"<GrandTotalAmount>4523.00</GrandTotalAmount>", "", 1)
	inv, err := parseCII([]byte(shrunk))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if inv.TotalGross != 0 || inv.TotalNet != 0 {
		t.Errorf("expected zero totals, got gross=%v net=%v", inv.TotalGross, inv.TotalNet)
	}
	if inv.Number != "INV-2026-0042" {
		t.Errorf("other fields regressed: %+v", inv)
	}
}
