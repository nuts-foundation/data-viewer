.PHONY: generate

generate:
	oapi-codegen -old-config-style -generate client,types -package network -o api/network/generated.go https://nuts-node.readthedocs.io/en/latest/_static/network/v1.yaml