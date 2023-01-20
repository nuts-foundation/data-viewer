package analyzers

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/nuts-foundation/go-did/did"
	"github.com/nuts-foundation/nuts-node/crypto/hash"
	networkAPI "github.com/nuts-foundation/nuts-node/network/api/v1"
	"github.com/nuts-foundation/nuts-node/network/dag"
	vdrAPI "github.com/nuts-foundation/nuts-node/vdr/api/v1"
	"io"
	"strings"
)

type DIDDocumentGraphAnalyzer struct {
	VDR     *vdrAPI.Client
	Network *networkAPI.Client
}

type node struct {
	tx    hash.SHA256Hash
	did   string
	notes []string
	lc    uint32
}

// Analyze renders a dotviz diagram of the DID, which contains all relevant transactions.
// You can specify multiple DIDs and/or transaction references (these need to be DID documents, however).
// Limitations:
// - It does not take into account controllers-of-controllers (only the first level is analyzed)
func (a DIDDocumentGraphAnalyzer) Analyze(ctx context.Context, didOrTXs []string) (string, error) {
	//
	var txsToAnalyze []hash.SHA256Hash
	var relevantDIDs []string
	for _, didOrTX := range didOrTXs {
		if strings.HasPrefix(didOrTX, "did:nuts:") {
			httpResponse, err := a.VDR.GetDID(ctx, didOrTX, &vdrAPI.GetDIDParams{})
			if err != nil {
				return "", fmt.Errorf("failed to get DID document: %w", err)
			}
			response, err := vdrAPI.ParseGetDIDResponse(httpResponse)
			if err != nil {
				return "", fmt.Errorf("failed to parse GetDID response: %w", err)
			}
			if response.JSON200 == nil {
				return "", fmt.Errorf("no DID document found (status=%d)", response.StatusCode())
			}
			txsToAnalyze = append(txsToAnalyze, response.JSON200.DocumentMetadata.SourceTransactions...)
			relevantDIDs = append(relevantDIDs, didOrTX)
			// We're interested in the controllers as well
			for _, controller := range response.JSON200.Document.Controller {
				relevantDIDs = append(relevantDIDs, controller.String())
			}
		} else {
			txRef, err := hash.ParseHex(didOrTX)
			if err != nil {
				return "", fmt.Errorf("invalid TX reference: %w", err)
			}
			_, document, err := a.readDIDDocument(ctx, txRef)
			if err != nil {
				return "", fmt.Errorf("failed to read DID document (tx=%s): %w", txRef, err)
			}
			if document == nil {
				return "", fmt.Errorf("specified TX %s does not contain a DID document", txRef)
			}
			txsToAnalyze = append(txsToAnalyze, txRef)
			relevantDIDs = append(relevantDIDs, document.ID.String())
			// We're interested in the controllers as well
			for _, controller := range document.Controller {
				relevantDIDs = append(relevantDIDs, controller.String())
			}
		}
	}

	edges := make(map[hash.SHA256Hash]map[hash.SHA256Hash]bool, 0)
	nodes := make(map[hash.SHA256Hash]node, 0)

	// Get the DID and all source TXs, these can be related (previous versions) or unrelated (the last TX of the DAG at that time);
	// we are only interested in the related TXs. We do this by checking whether the source TX is a related DID document,
	// meaning it has the correct content type and the DID inside it is either the document itself or (one of its) controllers.
	for _, txRef := range txsToAnalyze {
		err := a.analyze(ctx, hash.EmptyHash(), txRef, &relevantDIDs, edges, nodes)
		if err != nil {
			return "", err
		}
	}

	var lines []string
	lines = append(lines, "digraph {")
	for _, curr := range nodes {
		var label []string
		label = append(label, fmt.Sprintf("label=\"%s", curr.tx))
		label = append(label, curr.did)
		label = append(label, fmt.Sprintf("LC=%d", curr.lc))
		if len(curr.notes) > 0 {
			label = append(label, strings.Join(curr.notes, ","))
		}
		lines = append(lines, fmt.Sprintf(`	node_%s [%s"]`, curr.tx, strings.Join(label, `\n`)))
	}
	for left, rights := range edges {
		for right, _ := range rights {
			lines = append(lines, fmt.Sprintf(`	node_%s -> node_%s`, left, right))
		}
	}
	lines = append(lines, "}")
	return strings.Join(lines, "\n"), nil
}

func (a DIDDocumentGraphAnalyzer) analyze(ctx context.Context, referredBy hash.SHA256Hash, txRef hash.SHA256Hash, relevantDIDs *[]string, edges map[hash.SHA256Hash]map[hash.SHA256Hash]bool, nodes map[hash.SHA256Hash]node) error {
	// 1. Check if sourceTX is a DID document
	// 2. If so, check if it's the same DID or one of the controllers
	// If both are true, add it to the list and proceed to analyze
	tx, document, err := a.readDIDDocument(ctx, txRef)
	if err != nil {
		return fmt.Errorf("failed to read DID document (tx=%s): %w", txRef, err)
	}
	if document == nil {
		// TX does not contain a DID document
		return nil
	}
	// Same DID or one of the controllers?
	relevant := false
	for _, curr := range *relevantDIDs {
		if curr == document.ID.String() {
			relevant = true
			break
		}
		for _, controller := range document.Controller {
			if curr == controller.String() {
				relevant = true
				break
			}
		}
	}
	if !relevant {
		return nil
	}

	// Register the TX
	n := node{
		tx:  txRef,
		did: document.ID.String(),
		lc:  tx.Clock(),
	}
	if tx.SigningKey() != nil {
		n.notes = append(n.notes, "created")
	} else if tx.SigningKeyID() != "" {
		n.notes = append(n.notes, "update")
	}
	if len(document.Controller) == 0 && len(document.VerificationMethod) == 0 {
		n.notes = append(n.notes, "deactivated")
	}

	nodes[txRef] = n

	// Register edge
	if !referredBy.Empty() {
		rights := edges[txRef]
		if rights == nil {
			rights = make(map[hash.SHA256Hash]bool, 0)
		}
		rights[referredBy] = true
		edges[txRef] = rights
	}

	for _, prev := range tx.Previous() {
		err := a.analyze(ctx, txRef, prev, relevantDIDs, edges, nodes)
		if err != nil {
			return fmt.Errorf("failed to analyze transaction (tx=%s): %w", tx, err)
		}
	}
	return nil
}

// readDIDDocument reads the DID document from the given transaction. If the given transaction is not a DID document, it returns nil.
func (a DIDDocumentGraphAnalyzer) readDIDDocument(ctx context.Context, txRef hash.SHA256Hash) (dag.Transaction, *did.Document, error) {
	tx, payload, err := a.getTX(ctx, txRef)
	if err != nil {
		return nil, nil, err
	}
	// DID document?
	if tx.PayloadType() != "application/did+json" {
		return nil, nil, nil
	}
	document := &did.Document{}
	if err := json.Unmarshal(payload, document); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal DID document: %w", err)
	}
	return tx, document, nil
}

func (a DIDDocumentGraphAnalyzer) getTX(ctx context.Context, txRef hash.SHA256Hash) (dag.Transaction, []byte, error) {
	httpResponse, err := a.Network.GetTransaction(ctx, txRef.String())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get transaction: %w", err)
	}
	data, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read HTTP response: %w", err)
	}
	tx, err := dag.ParseTransaction(data)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse transaction: %w", err)
	}
	httpResponse, err = a.Network.GetTransactionPayload(ctx, txRef.String())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get transaction payload: %w", err)
	}
	payload, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read transaction payload response: %w", err)
	}
	return tx, payload, nil
}
