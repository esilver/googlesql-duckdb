package internal

import (
	"fmt"

	"github.com/goccy/googlesqlite/internal/functions/longtail"
	netfn "github.com/goccy/googlesqlite/internal/functions/net"
	"github.com/goccy/googlesqlite/internal/value"
)

// netBindFuncs maps the formatter-lowered scalar name for each NET.*
// function to its pure-Go BindFunction body. The names are the bare
// storage names the formatter emits (NET.MAKE_NET -> make_net, etc.),
// matching the UDF names registered in the root package against DuckDB.
//
// Reusing the same bodies that the SQLite-coupled function_register_*
// path uses keeps semantics (arg order, range checks, NULL handling)
// identical across both backends; only the value-layer plumbing differs.
var netBindFuncs = map[string]func(...value.Value) (value.Value, error){
	"host":                netfn.BindNetHost,
	"ip_from_string":      netfn.BindNetIpFromString,
	"safe_ip_from_string": netfn.BindNetSafeIpFromString,
	"ip_to_string":        netfn.BindNetIpToString,
	"ip_net_mask":         netfn.BindNetIpNetMask,
	"ip_trunc":            netfn.BindNetIpTrunc,
	"ipv4_from_int64":     netfn.BindNetIpv4FromInt64,
	"ipv4_to_int64":       netfn.BindNetIpv4ToInt64,
	"public_suffix":       netfn.BindNetPublicSuffix,
	"reg_domain":          netfn.BindNetRegDomain,
	"ip_in_net":           longtail.BindNetIpInNet,
	"parse_ip":            longtail.BindNetParseIp,
	"format_ip":           longtail.BindNetFormatIp,
	"parse_packed_ip":     longtail.BindNetParsePackedIp,
	"format_packed_ip":    longtail.BindNetFormatPackedIp,
	"make_net":            longtail.BindNetMakeNet,
}

// NetUDFNames returns the lowered scalar names of every NET.* function
// bridged to DuckDB, so the root-package registrar can install one
// scalar UDF per name without duplicating the list.
func NetUDFNames() []string {
	names := make([]string, 0, len(netBindFuncs))
	for name := range netBindFuncs {
		names = append(names, name)
	}
	return names
}

// GoogleSQLNetCall is the DuckDB-facing bridge for the NET.* scalar
// UDFs. DuckDB passes each argument as a raw driver value: a VARCHAR
// holding the value-layer base64-JSON envelope for STRING/BYTES inputs
// (round-tripped from a nested NET call or a string literal), or a
// native integer for a numeric literal argument. We decode every
// argument back into a value.Value, delegate to the shared pure-Go
// body, then re-encode the result into the wire form DuckDB stores:
// a Go primitive for INT64/BOOL results, or the base64-JSON envelope
// string for STRING/BYTES results.
func GoogleSQLNetCall(name string, args ...any) (any, error) {
	bind, ok := netBindFuncs[name]
	if !ok {
		return nil, fmt.Errorf("googlesqlite: unknown net udf %q", name)
	}
	vals := make([]value.Value, 0, len(args))
	for _, a := range args {
		v, err := value.DecodeValue(a)
		if err != nil {
			return nil, fmt.Errorf("googlesqlite net %s: decode arg: %w", name, err)
		}
		vals = append(vals, v)
	}
	out, err := bind(vals...)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, nil
	}
	return value.EncodeValue(out)
}
