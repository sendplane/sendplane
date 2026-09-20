package ingest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sendplane/sendplane/host"
)

func TestCSVToNDJSON(t *testing.T) {
	in := "Email,Name,locale,unsubscribe_url,plan,seats\n" +
		"a@example.com,Ada,ko,https://host.example/u/1,pro,3\n" +
		"b@example.com, Bob ,,,,\n"
	var out strings.Builder
	if err := CSVToNDJSON(strings.NewReader(in), &out, ColumnMapping{}); err != nil {
		t.Fatal(err)
	}
	want := `{"email":"a@example.com","name":"Ada","locale":"ko","vars":{"plan":"pro","seats":"3"},"unsubscribe_url":"https://host.example/u/1"}` + "\n" +
		`{"email":"b@example.com","name":"Bob"}` + "\n"
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}

	// The conversion feeds straight back into Ingest.
	ing, _, c := fixture(t, host.Limits{})
	res, err := ing.Ingest(context.Background(), c.ID, "", strings.NewReader(out.String()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 2 || res.Invalid != 0 {
		t.Fatalf("got %+v: %+v", res, res.Errors)
	}
}

func TestCSVToNDJSONMapping(t *testing.T) {
	in := "주소,이름,tier\nc@example.com,Cho,gold\n"
	var out strings.Builder
	err := CSVToNDJSON(strings.NewReader(in), &out, ColumnMapping{Email: "주소", Name: "이름"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"email":"c@example.com","name":"Cho","vars":{"tier":"gold"}}` + "\n"
	if out.String() != want {
		t.Fatalf("got %s", out.String())
	}
}

func TestCSVToNDJSONErrors(t *testing.T) {
	var out strings.Builder
	err := CSVToNDJSON(strings.NewReader("name,plan\nAda,pro\n"), &out, ColumnMapping{})
	if !errors.Is(err, ErrNoEmailColumn) {
		t.Fatalf("err = %v, want ErrNoEmailColumn", err)
	}

	out.Reset()
	if err := CSVToNDJSON(strings.NewReader(""), &out, ColumnMapping{}); err != nil {
		t.Fatalf("empty file: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("empty file produced %q", out.String())
	}

	out.Reset()
	err = CSVToNDJSON(strings.NewReader("email,plan\na@example.com\n"), &out, ColumnMapping{})
	if err == nil {
		t.Fatal("ragged row accepted")
	}
}

// TestCSVEmptyEmailStaysInvalid: a row with an empty address is written out
// and counted as invalid by Ingest, with its line number, rather than dropped
// here where the uploader would never hear about it. A blank CSV line is not a
// row at all and encoding/csv skips it.
func TestCSVEmptyEmailStaysInvalid(t *testing.T) {
	var out strings.Builder
	in := "email,name\na@example.com,Ada\n\n,Nobody\nb@example.com,Bob\n"
	if err := CSVToNDJSON(strings.NewReader(in), &out, ColumnMapping{}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), "\n"); n != 3 {
		t.Fatalf("wrote %d lines:\n%s", n, out.String())
	}
	ing, _, c := fixture(t, host.Limits{})
	res, err := ing.Ingest(context.Background(), c.ID, "", strings.NewReader(out.String()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 2 || res.Invalid != 1 {
		t.Fatalf("got %+v: %+v", res, res.Errors)
	}
	if res.Errors[0].Line != 2 {
		t.Fatalf("invalid line = %d, want 2", res.Errors[0].Line)
	}
}
