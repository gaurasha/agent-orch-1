package store

import "fmt"

// VerifyChain re-computes a tenant's audit hash chain and reports the first
// record that does not match.
//
// This is what makes the audit log worth having. Any store can hold rows; the
// question an auditor actually asks is "can an operator with database access
// quietly rewrite what an agent did?". With the chain, editing record N
// invalidates N and every record after it, so tampering requires rewriting the
// entire tail - and if the head hash is periodically published somewhere the
// operator does not control (a transparency log, a different account's object
// store, a customer-held receipt), it requires forging that too.
func VerifyChain(recs []AuditRecord) error {
	prev := GenesisHash
	var lastSeq int64
	for i, r := range recs {
		if i > 0 && r.Seq != lastSeq+1 {
			return fmt.Errorf("audit chain gap before seq %d (previous %d): records were deleted", r.Seq, lastSeq)
		}
		if r.PrevHash != prev {
			return fmt.Errorf("audit chain broken at seq %d: prev_hash=%s, expected %s", r.Seq, r.PrevHash, prev)
		}
		want, err := HashAudit(r)
		if err != nil {
			return fmt.Errorf("audit chain: rehash seq %d: %w", r.Seq, err)
		}
		if want != r.Hash {
			return fmt.Errorf("audit record seq %d was modified after it was written (hash mismatch)", r.Seq)
		}
		prev, lastSeq = r.Hash, r.Seq
	}
	return nil
}
