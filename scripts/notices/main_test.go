package main

import (
	"os"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"apache full", "Apache License\nVersion 2.0, January 2004\nhttp://www.apache.org/licenses/", "Apache-2.0"},
		{"apache header", "Copyright 2021 X\n\nLicensed under the Apache License, Version 2.0 (the \"License\");\nyou may not", "Apache-2.0"},
		{"mit", "Permission is hereby granted, free of charge, to any person obtaining a copy of this software", "MIT"},
		{"isc", "Permission to use, copy, modify, and/or distribute this software for any purpose with or without fee is hereby granted", "ISC"},
		{"bsd2", "Redistribution and use in source and binary forms, with or without modification, are permitted", "BSD-2-Clause"},
		{"bsd3", "Redistribution and use in source and binary forms, with or without modification, are permitted\nNeither the name of X nor the names of its contributors may be used to endorse", "BSD-3-Clause"},
		{"two licences", "Permission is hereby granted, free of charge, to any person obtaining a copy\nApache License\nVersion 2.0, January 2004", "Apache-2.0 AND MIT"},
		{"unknown", "All rights reserved.", ""},
	}
	for _, c := range cases {
		if got := classify(c.text); got != c.want {
			t.Errorf("%s: classify = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestReadTextNormalisesLineEnds(t *testing.T) {
	p := t.TempDir() + "/LICENSE"
	if err := os.WriteFile(p, []byte("a\r\nb\r\n\r\n\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readText(p)
	if err != nil || got != "a\nb\n" {
		t.Fatalf("readText = %q, %v; want %q", got, err, "a\nb\n")
	}
}
