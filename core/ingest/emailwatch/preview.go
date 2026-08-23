package emailwatch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-imap"

	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
)

const (
	previewMessageLimit = 25
	previewSampleLimit  = 5
)

type PreviewSample struct {
	From        string   `json:"from,omitempty"`
	Subject     string   `json:"subject,omitempty"`
	Attachments []string `json:"attachments,omitempty"`
}

type PreviewResult struct {
	Inspected int             `json:"inspected"`
	Matched   int             `json:"matched"`
	Samples   []PreviewSample `json:"samples"`
}

// PreviewPolicy evaluates at most 25 recent, unchecked messages using envelope
// metadata and IMAP BODYSTRUCTURE. It does not download or store message bodies.
func (w *Watcher) PreviewPolicy(ctx context.Context, policy emailaccounts.IntakePolicy) (*PreviewResult, error) {
	policy, err := emailaccounts.NormalizeIntakePolicy(policy)
	if err != nil {
		return nil, err
	}
	c, err := w.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Logout() }()

	if _, err := c.Select(w.account.Folder, true); err != nil {
		return nil, fmt.Errorf("select folder: %w", err)
	}
	criteria := imap.NewSearchCriteria()
	uidSet := new(imap.SeqSet)
	uidSet.AddRange(w.account.LastUIDSeen+1, 0)
	criteria.Uid = uidSet
	if ts := w.account.SyncSince; ts != nil && *ts > 0 {
		criteria.Since = time.Unix(*ts, 0).UTC()
	}
	uids, err := c.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	if len(uids) > previewMessageLimit {
		uids = uids[len(uids)-previewMessageLimit:]
	}
	result := &PreviewResult{Samples: []PreviewSample{}}
	if len(uids) == 0 {
		return result, nil
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(uids...)
	messages := make(chan *imap.Message, previewMessageLimit)
	done := make(chan error, 1)
	go func() {
		done <- c.UidFetch(seqset, []imap.FetchItem{
			imap.FetchEnvelope,
			imap.FetchBodyStructure,
		}, messages)
	}()
	for message := range messages {
		if message == nil {
			continue
		}
		names := bodyStructureAttachmentNames(message.BodyStructure)
		result.Inspected++
		if evaluatePolicy(policy, message.Envelope, names) == "" {
			continue
		}
		result.Matched++
		if len(result.Samples) < previewSampleLimit {
			result.Samples = append(result.Samples, PreviewSample{
				From:        envelopeSender(message.Envelope),
				Subject:     envelopeSubject(message.Envelope),
				Attachments: names,
			})
		}
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("fetch preview: %w", err)
	}
	return result, nil
}

func bodyStructureAttachmentNames(structure *imap.BodyStructure) []string {
	if structure == nil {
		return nil
	}
	var names []string
	structure.Walk(func(_ []int, part *imap.BodyStructure) bool {
		if part == nil || strings.EqualFold(strings.TrimSpace(part.Disposition), "inline") {
			return true
		}
		filename, _ := part.Filename()
		if filename = strings.TrimSpace(filename); filename != "" {
			names = append(names, filename)
		}
		return true
	})
	return names
}
