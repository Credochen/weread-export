package main

import (
	"reflect"
	"testing"
)

func TestSwapPositions(t *testing.T) {
	for _, v := range decodeVectors {
		got := swapPositions(v.in)
		if len(got) == 0 && len(v.pos) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, v.pos) {
			t.Fatalf("swapPositions(%q) = %v, want %v", v.in, got, v.pos)
		}
	}
}

func TestReverseSwaps(t *testing.T) {
	for _, v := range decodeVectors {
		got := reverseSwaps(v.in, v.pos)
		if got != v.reordered {
			t.Fatalf("reverseSwaps(%q) = %q, want %q", v.in, got, v.reordered)
		}
	}
}

func TestDecodePayload(t *testing.T) {
	for _, v := range decodeVectors {
		got, err := DecodePayload(v.payload)
		if err != nil {
			t.Fatalf("DecodePayload(%q) error: %v", v.payload, err)
		}
		if got != v.decoded {
			t.Fatalf("DecodePayload(%q) = %q, want %q", v.payload, got, v.decoded)
		}
	}
}

func TestWereadE(t *testing.T) {
	cases := map[string]string{
		"43208843": "c9c321c07293508bc9c79df",
		"2":        "c81322c012c81e728d9d180",
		"119":      "07e323f027707e1cd7dc674",
	}
	for in, want := range cases {
		if got := WereadE(in); got != want {
			t.Fatalf("WereadE(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWereadSign(t *testing.T) {
	q := "b=x&c=y&ct=1700000000&pc=q&prevChapter=false&ps=z&r=1&sc=1&st=0"
	if got := WereadSign(q); got != "1f5e5484" {
		t.Fatalf("WereadSign(%q) = %q, want 1f5e5484", q, got)
	}
}

func TestWebAppID(t *testing.T) {
	ua := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36 Edg/135.0.0.0"
	if got := WebAppID(ua); got != "wb115321887466h529830856" {
		t.Fatalf("WebAppID = %q, want wb115321887466h529830856", got)
	}
}
