# Lists the CEL rules (+kubebuilder:validation:XValidation markers) of the Go types
# it is given, as a Markdown bullet list: crd-ref-docs does not render them, and
# they are what the API server answers with when an object is refused. Run by
# `make crd-ref-docs` over api/v1alpha1/*_types.go; POSIX awk only, so the output
# is the same under mawk and gawk.
#
# A marker applies to the declaration that follows its comment block: a type, or a
# struct field (named by its json tag).

function flush(subject,    i) {
	for (i = 1; i <= n; i++) {
		printf "- `%s`: %s\n", subject, msg[i]
	}
	n = 0
}

/^[ \t]*\/\// {
	if (index($0, "+kubebuilder:validation:XValidation:") > 0) {
		line = $0
		start = index(line, "message=\"")
		if (start == 0) {
			next
		}
		rest = substr(line, start + 9)
		end = index(rest, "\",")
		if (end == 0) {
			end = length(rest)   # message is the last key: drop the closing quote
		}
		text = substr(rest, 1, end - 1)
		if (end == length(rest) && substr(text, length(text)) == "\"") {
			text = substr(text, 1, length(text) - 1)
		}
		gsub(/\\\\/, "\\&#92;", text)
		gsub(/</, "\\&lt;", text)
		gsub(/>/, "\\&gt;", text)
		msg[++n] = text
	}
	next
}

/^[ \t]*$/ { next }

/^type [A-Za-z0-9_]+/ {
	cur = $2
	flush(cur)
	next
}

/^[ \t]+[A-Z][A-Za-z0-9_]*[ \t].*`json:"/ {
	tag = $0
	sub(/.*`json:"/, "", tag)
	sub(/[",].*/, "", tag)
	flush(cur "." tag)
	next
}

{ n = 0 }
