// SPDX-License-Identifier: AGPL-3.0-or-later

package presetfile

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

const header = `format = "suchi-taxonomy/v1"
id = "example"
version = 1
name = "Example"
market = "global"
language = "en"
story = "A filing map for personal records."
`

func TestV1Compatibility(t *testing.T) {
	a := mustParseFile(t, "testdata/valid.toml", FormatTOML)
	b := mustParseFile(t, "testdata/valid.huml", FormatHuML)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("serialization changes meaning:\n%+v\n%+v", a, b)
	}
	if a.Inbox != 49 || len(a.Areas) != 2 || a.Areas[1].Code != 40 {
		t.Fatalf("generated structure: %+v", a)
	}
	for _, f := range []SerFormat{FormatHuML, FormatTOML} {
		data, err := Marshal(a, f)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Parse(data, f)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, got) {
			t.Fatalf("%s round trip changed taxonomy", f)
		}
	}
}

func TestRawShapeCannotDisappearDuringNormalization(t *testing.T) {
	cases := map[string]string{
		"missing areas":        "",
		"inbox":                "inbox = 49\nareas = []",
		"system":               "system = false\nareas = []",
		"empty system":         "system = ''\nareas = []",
		"system list":          "system = ['S01']\nareas = []",
		"system object":        "system = {code='S01'}\nareas = []",
		"system number":        "system = 1\nareas = []",
		"multiple systems":     "systems = ['S01','S02']\nareas = []",
		"documents":            "documents = [{id=147}]\nareas = []",
		"members":              "members = [1]\nareas = []",
		"flat areas":           "flat = true\nareas = []\ncategories = [{name='A'}]",
		"tree categories":      "areas = []\ncategories = []",
		"flat empty":           "flat = true\ncategories = []",
		"flat excess":          "flat = true\ncategories = [" + strings.Repeat("{name='A'},", 10) + "]",
		"flat advisory code":   "flat = true\ncategories = [{code=1,name='A'}]",
		"flat explicit zero":   "flat = true\ncategories = [{code=0,name='A'}]",
		"reserved index":       "areas = [{code=0,name='Index',categories=[]}]",
		"reserved system":      "areas = [{code=40,name='System',categories=[{code=49,name='Inbox'}]}]",
		"category zero":        "areas = [{code=10,name='A',categories=[{code=10,name='B'}]}]",
		"duplicate area":       "areas = [{code=10,name='A',categories=[]},{code=10,name='B',categories=[]}]",
		"duplicate category":   "areas = [{code=10,name='A',categories=[{code=11,name='B'},{code=11,name='C'}]}]",
		"unsupported category": "areas = [{code=10,name='A',categories=[{code=11,name='B',sensitivity='public'}]}]",
		"unsupported seeds":    "areas = []\n[seeds]\ntags = ['a']",
		"fractional code":      "areas = [{code=10.0,name='A',categories=[]}]",
		"typed keyword":        "areas = [{code=10,name='A',categories=[{code=11,name='B',keywords=[42]}]}]",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(header+body), FormatTOML); err == nil {
				t.Fatal("invalid raw input accepted")
			}
		})
	}
}

func TestBlankAndFlatRoundTrip(t *testing.T) {
	for _, body := range []string{"areas = []", "flat = true\ncategories = [{name='Bills'},{code=12,name='Receipts'}]"} {
		pf, err := Parse([]byte(header+body), FormatTOML)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range []SerFormat{FormatTOML, FormatHuML} {
			data, err := Marshal(pf, f)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Parse(data, f)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(pf, got) {
				t.Fatalf("round trip changed blank/flat: %s", data)
			}
		}
	}
}

func TestStrictHuMLAndFormatFirst(t *testing.T) {
	valid := string(readFile(t, "testdata/valid.huml"))
	for _, input := range []string{
		valid + "inbox: 49\n",
		valid + "system: null\n",
		strings.Replace(valid, "code: 11", "code: 11\n        code: 12", 1),
		strings.Replace(valid, "document_type: \"certificate\"", "document_type: \"certificate\"\n            document_type: \"invoice\"", 1),
		strings.Replace(valid, "document_type: \"certificate\"", "owner_id: 1", 1),
		strings.Replace(valid, "version: 1", "version: 1.5", 1),
		strings.Replace(valid, "name: \"UK landlord\"", "name: null", 1),
		strings.Replace(valid, "filter_content_matching: \"gas safety\"", "filter_future: \"gas safety\"", 1),
	} {
		if _, err := Parse([]byte(input), FormatHuML); err == nil {
			t.Fatalf("accepted invalid HuML: %s", input)
		}
	}
	for _, f := range []SerFormat{FormatTOML, FormatHuML} {
		input := "format = 'future/v2'\nunknown = true"
		if f == FormatHuML {
			input = "format: \"future/v2\"\nunknown: true"
		}
		_, err := Parse([]byte(input), f)
		if err == nil || !strings.Contains(err.Error(), "format") || strings.Contains(err.Error(), "unknown") {
			t.Fatalf("format dispatch: %v", err)
		}
	}
}

func TestPortableActionsAreExactAndTyped(t *testing.T) {
	base := header + "areas = [{code=10,name='A',categories=[{code=11,name='B'}]}]\n[[seeds.automations]]\nname='Rule'\ntrigger={type=2,filter_title_matching='Invoice',filter_filename='*.pdf',filter_has_tag='incoming',filter_email_has_attachment=false}\nactions=["
	good := []string{
		"{kind='assign_title',params={template='Invoice'}}",
		"{kind='assign_tags',params={tags=['a','b']}}",
		"{kind='assign_tags',params={tag='a'}}",
		"{kind='assign_correspondent',params={correspondent='Example'}}",
		"{kind='assign_document_type',params={document_type='Invoice'}}",
		"{kind='assign_jd_category',params={jd_category_code=11}}",
	}
	for _, action := range good {
		pf, err := Parse([]byte(base+action+"]"), FormatTOML)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range []SerFormat{FormatHuML, FormatTOML} {
			if _, err := Marshal(pf, f); err != nil {
				t.Fatal(err)
			}
		}
	}
	bad := []string{
		"{kind='assign_title',params={template='Title',owner_id=1}}",
		"{kind='assign_tags',params={tag='a',tags=['b']}}",
		"{kind='assign_tags',params={tags=['a',1]}}",
		"{kind='assign_tags',params={tags=[]}}",
		"{kind='assign_jd_category',params={jd_category_code=11.5}}",
		"{kind='assign_jd_category',params={jd_category_code=49}}",
		"{kind='assign_jd_category',params={jd_category_code=12}}",
		"{kind='assign_jd_category',params={jd_category_code=11,system='S02'}}",
		"{kind='assign_jd_category',params={jd_category_code=11,system_id=2}}",
		"{kind='assign_owner',params={owner_id=1}}",
		"{kind='assign_document_type',params={document_type_id=1}}",
		"{kind='assign_title',params={template=' '}}",
	}
	for _, action := range bad {
		if _, err := Parse([]byte(base+action+"]"), FormatTOML); err == nil {
			t.Fatalf("accepted %s", action)
		}
	}
	valid := base + good[0] + "]"
	for _, input := range []string{strings.Replace(valid, "type=2", "type=4", 1), strings.Replace(valid, "filter_title_matching='Invoice'", "filter_title_matching='['", 1), strings.Replace(valid, "filter_filename='*.pdf'", "filter_filename='['", 1), base + "]", valid + "\n[[seeds.automations]]\nname='Rule'\ntrigger={type=2}\nactions=[" + good[0] + "]"} {
		if _, err := Parse([]byte(input), FormatTOML); err == nil {
			t.Fatal("invalid starter accepted")
		}
	}
}

func TestMarshalRefusesLegacyReservedContent(t *testing.T) {
	for _, mutate := range []func(*PresetFile){
		func(p *PresetFile) { p.Inbox = 11 },
		func(p *PresetFile) { p.Areas[1].Name = "Custom" },
		func(p *PresetFile) {
			p.Areas[1].Categories = append(p.Areas[1].Categories, Category{Code: 41, Name: "Old"})
		},
		func(p *PresetFile) { p.Areas = append(p.Areas, Area{Code: 0, Name: "Index", Categories: []Category{}}) },
	} {
		pf := mustParseFile(t, "testdata/valid.toml", FormatTOML)
		mutate(pf)
		if _, err := Marshal(pf, FormatHuML); err == nil {
			t.Fatal("legacy reserved structure silently removed")
		}
	}
}

func TestSerializationAndReadBoundaries(t *testing.T) {
	for _, ext := range []string{".yaml", ".yml", ".json", "", ".txt"} {
		if _, err := ParseFromExt(readFile(t, "testdata/valid.toml"), ext); err == nil {
			t.Fatalf("accepted %q", ext)
		}
	}
	for _, data := range [][]byte{nil, []byte(strings.Repeat("x", MaxFileSize+1)), {0xff}} {
		if _, err := Parse(data, FormatHuML); err == nil {
			t.Fatal("invalid document accepted")
		}
	}
	if _, err := Parse(readFile(t, "testdata/valid.toml"), ""); err == nil {
		t.Fatal("sniffed TOML")
	}
}

func mustParseFile(t *testing.T, path string, f SerFormat) *PresetFile {
	t.Helper()
	pf, err := Parse(readFile(t, path), f)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return pf
}
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSystemHeaderRoundTripAndStrictCodes(t *testing.T) {
	for _, code := range []string{"S01", "S02", "A00", "Z99"} {
		a, err := Parse([]byte(header+"system = '"+code+"'\nareas = []"), FormatTOML)
		if err != nil {
			t.Fatal(err)
		}
		if a.System != code {
			t.Fatalf("system lost: %+v", a)
		}
		for _, format := range []SerFormat{FormatHuML, FormatTOML} {
			data, err := Marshal(a, format)
			if err != nil {
				t.Fatal(err)
			}
			b, err := Parse(data, format)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("%s changed system taxonomy", format)
			}
		}
	}
	for _, code := range []string{"s01", "AUD", "S1", "S001", " S01", "S01 ", "S01.13", "001", "Å01"} {
		for _, format := range []SerFormat{FormatHuML, FormatTOML} {
			base := string(readFile(t, "testdata/valid.huml"))
			input := base + "system: \"" + code + "\"\n"
			if format == FormatTOML {
				input = header + "system = '" + code + "'\nareas = []"
			}
			if _, err := Parse([]byte(input), format); err == nil {
				t.Fatalf("accepted system %q in %s", code, format)
			}
		}
	}
	unnamed, err := Parse([]byte(header+"areas = []"), FormatTOML)
	if err != nil {
		t.Fatal(err)
	}
	if unnamed.System != "" {
		t.Fatal("unprefixed file acquired a system")
	}
}
