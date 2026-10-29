package main

import "encoding/json"

func jsonIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }
