package hl7

import (
	"reflect"
	"testing"
)

func TestER7OpaqueEscapePreservesFieldStructure(t *testing.T) {
	const defaultHeader = `MSH|^~\&|REG|NORTH|VIEW|SOUTH|20261115100500||ADT^A08^ADT_A01|ESCAPE-1|P|2.5.1`
	const customHeader = `MSH*%!?@*REG*NORTH*VIEW*SOUTH*20261115100500**ADT%A08%ADT_A01*ESCAPE-2*P*2.5.1`
	for _, tc := range []struct {
		name   string
		header string
		wire   string
		first  Field
	}{
		{
			name: "opaque escape without embedded separators", header: defaultHeader,
			wire: `ZXY|\Zopaque\|TERMINAL`, first: Text(`\Zopaque\`),
		},
		{
			name: "standard escapes", header: defaultHeader,
			wire: `ZXY|A\F\B\S\C\R\D\T\E|TERMINAL`, first: Text("A|B^C~D&E"),
		},
		{
			name: "field and component separators", header: defaultHeader,
			wire:  `ZXY|\Zopaque|inside^literal\|TERMINAL`,
			first: Text(`\Zopaque|inside^literal\`),
		},
		{
			name: "repetition and subcomponent separators", header: defaultHeader,
			wire:  `ZXY|head&\Zopaque~inside&literal\^tail~next|TERMINAL`,
			first: Field{{{"head", `\Zopaque~inside&literal\`}, {"tail"}}, {{"next"}}},
		},
		{
			name: "custom separators", header: customHeader,
			wire:  `ZXY*before?Zopaque*inside%literal!second@third?after*TERMINAL`,
			first: Text(`before?Zopaque*inside%literal!second@third?after`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := Parse(message(tc.header, tc.wire))
			if err != nil {
				t.Fatalf("parse ER7: %v", err)
			}
			segment, ok := parsed.Segment("ZXY")
			if !ok {
				t.Fatal("parsed message has no ZXY segment")
			}
			want := []Field{tc.first, Text("TERMINAL")}
			if !reflect.DeepEqual(segment.Fields, want) {
				t.Errorf("ZXY fields = %#v, want %#v", segment.Fields, want)
			}
		})
	}
}
