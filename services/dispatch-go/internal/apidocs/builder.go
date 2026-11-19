// Package apidocs builds HexaGo's OpenAPI 3.1 document (every public
// dispatch-go endpoint and the agents-py endpoints the gateway proxies) and
// serves it at /openapi.json with Swagger UI at /docs.
package apidocs

import "strconv"

// M is a JSON object.
type M = map[string]any

func ref(name string) M { return M{"$ref": "#/components/schemas/" + name} }

func str(desc string) M {
	s := M{"type": "string"}
	if desc != "" {
		s["description"] = desc
	}
	return s
}

func enum(desc string, vals ...string) M {
	s := str(desc)
	s["enum"] = vals
	return s
}

func num(desc string) M {
	s := M{"type": "number"}
	if desc != "" {
		s["description"] = desc
	}
	return s
}

func integer(desc string) M {
	s := M{"type": "integer"}
	if desc != "" {
		s["description"] = desc
	}
	return s
}

func boolean(desc string) M {
	s := M{"type": "boolean"}
	if desc != "" {
		s["description"] = desc
	}
	return s
}

func dt(desc string) M {
	s := str(desc)
	s["format"] = "date-time"
	return s
}

// orNull allows null besides schema s (JSON Schema 2020-12 / OpenAPI 3.1 style).
func orNull(s M) M {
	if t, ok := s["type"].(string); ok {
		c := M{}
		for k, v := range s {
			c[k] = v
		}
		c["type"] = []string{t, "null"}
		if e, ok := c["enum"].([]string); ok {
			vals := make([]any, 0, len(e)+1)
			for _, v := range e {
				vals = append(vals, v)
			}
			c["enum"] = append(vals, nil)
		}
		return c
	}
	return M{"anyOf": []M{s, {"type": "null"}}}
}

func arr(items M) M { return M{"type": "array", "items": items} }

func obj(required []string, props M) M {
	s := M{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func mapOf(v M) M { return M{"type": "object", "additionalProperties": v} }

func anyObj(desc string) M {
	s := M{"type": "object"}
	if desc != "" {
		s["description"] = desc
	}
	return s
}

func withDesc(s M, desc string) M {
	c := M{}
	for k, v := range s {
		c[k] = v
	}
	c["description"] = desc
	return c
}

// ---- operations ----

type opt func(M)

// op builds an operation. scope "" marks a public operation (no key needed).
func op(tag, id, summary, scope string, opts ...opt) M {
	o := M{"tags": []string{tag}, "operationId": id, "summary": summary, "responses": M{}}
	if scope == "" {
		o["security"] = []M{{}}
		o["description"] = "Public: no API key needed."
	} else {
		o["x-required-scope"] = scope
		o["description"] = "Scope `" + scope + "` when called with an API key (keyless first-party calls are allowed)."
		resp := o["responses"].(M)
		resp["401"] = M{"$ref": "#/components/responses/Unauthorized"}
		resp["403"] = M{"$ref": "#/components/responses/Forbidden"}
		resp["429"] = M{"$ref": "#/components/responses/RateLimited"}
	}
	for _, f := range opts {
		f(o)
	}
	return o
}

func desc(text string) opt {
	return func(o M) { o["description"] = text + "\n\n" + o["description"].(string) }
}

func param(in, name, typ, d string, required bool) opt {
	return func(o M) {
		ps, _ := o["parameters"].([]M)
		o["parameters"] = append(ps, M{"name": name, "in": in, "required": required, "description": d,
			"schema": M{"type": typ}})
	}
}

func query(name, typ, d string) opt { return param("query", name, typ, d, false) }
func path(name, d string) opt       { return param("path", name, "string", d, true) }

func body(schema M, required bool) opt {
	return func(o M) {
		o["requestBody"] = M{"required": required, "content": M{"application/json": M{"schema": schema}}}
	}
}

func rawBody(contentType, d string) opt {
	return func(o M) {
		o["requestBody"] = M{"required": true, "description": d,
			"content": M{contentType: M{"schema": M{"type": "string", "format": "binary"}}}}
	}
}

// ok adds a JSON response.
func ok(code int, d string, schema M) opt {
	return func(o M) {
		r := M{"description": d}
		if schema != nil {
			r["content"] = M{"application/json": M{"schema": schema}}
		}
		o["responses"].(M)[strconv.Itoa(code)] = r
	}
}

// content adds a non-JSON response.
func content(code int, d, contentType string) opt {
	return func(o M) {
		o["responses"].(M)[strconv.Itoa(code)] = M{"description": d,
			"content": M{contentType: M{"schema": M{"type": "string"}}}}
	}
}

// fails adds error responses with the Error body.
func fails(codes ...int) opt {
	return func(o M) {
		for _, c := range codes {
			o["responses"].(M)[strconv.Itoa(c)] = M{"description": statusText(c),
				"content": M{"application/json": M{"schema": ref("Error")}}}
		}
	}
}

func statusText(c int) string {
	switch c {
	case 400:
		return "Invalid request"
	case 404:
		return "Not found"
	case 409:
		return "Conflict with the current state"
	case 413:
		return "Payload too large"
	case 415:
		return "Unsupported media type"
	case 422:
		return "Validation error"
	case 502:
		return "Upstream (agents-py or a dependency) failed"
	case 503:
		return "Unavailable (feature off or backend down)"
	}
	return "Error"
}

func proxied() opt {
	return func(o M) {
		o["x-served-by"] = "agents-py"
		o["description"] = o["description"].(string) + " Served by agents-py through the gateway proxy."
	}
}
