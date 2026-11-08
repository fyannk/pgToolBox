

## Cross-field rules

Rules written in CEL (`x-kubernetes-validations`) that the API server enforces and
that the Validation column cannot express, with the type or field each applies to.
They are collected from the same Go markers (`+kubebuilder:validation:XValidation`)
by `docs/site/crd-ref-docs/cel-rules.awk`.

