package server

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/juev/hledger-lsp/internal/formatter"
	"github.com/juev/hledger-lsp/internal/lsputil"
	"github.com/juev/hledger-lsp/internal/parser"
)

func requestInlayHints(t *testing.T, srv *Server, content string) []protocol.InlayHint {
	t.Helper()
	docURI := uri.URI("file:///test.journal")
	srv.StoreDocument(docURI, content)
	hints, err := srv.InlayHint(context.Background(), &protocol.InlayHintParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
		Range:        protocol.Range{End: protocol.Position{Line: 100}},
	})
	require.NoError(t, err)
	return hints
}

func labelText(t *testing.T, hint protocol.InlayHint) string {
	t.Helper()
	label, ok := hint.Label.(protocol.String)
	require.True(t, ok, "label must be a plain string, got %T", hint.Label)
	return string(label)
}

// The hint is right when writing its text in place gives a line the formatter
// has no wish to move.
func TestInlayHint_InferredAmountSitsWhereTheFormatterWouldPutIt(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		content string
	}{
		{"right, commodity on the right", "right", "2024-01-15 shop\n    expenses:food:groceries  1250.00 USD\n    assets:cash\n"},
		{"right, commodity on the left", "right", "2024-01-15 shop\n    expenses:food:groceries  $1250.00\n    assets:cash\n"},
		{"left", "left", "2024-01-15 shop\n    expenses:food:groceries  1250.00 USD\n    assets:cash\n"},
		// The second transaction keeps the decimal column put: the inferred amount
		// is a sign wider than 1250.00, and if it were the widest amount in the
		// file, writing it out would move every amount, not just this one.
		{"decimal", "decimal", "2024-01-15 shop\n    expenses:food:groceries  1250.00 USD\n    assets:cash\n\n2024-01-16 big\n    expenses:car  99999.00 USD\n    assets:bank  -99999.00 USD\n"},
		{"cyrillic", "right", "2024-01-15 покупка\n    расходы:продукты:магазин  1250.00 ₽\n    активы:наличные\n"},
		{"cjk width", "right", "2024-01-15 買い物\n    費用:食品:スーパーマーケット  1250.00 円\n    資産:現金\n"},
		{"cjk width, left", "left", "2024-01-15 買い物\n    費用:食品:スーパーマーケット  1250.00 円\n    資産:現金\n"},
		{"balanced virtual brackets", "right", "2024-01-15 shop\n    [budget:food:groceries]  1250.00 USD\n    [budget:cash]\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewServer()
			settings := srv.getSettings()
			settings.Formatting.AmountAlignmentMode = tt.mode
			settings.InlayHints.InferredAmountAlignment = inferredAlignFormatter
			srv.setSettings(settings)

			hints := requestInlayHints(t, srv, tt.content)
			require.Len(t, hints, 1)
			require.NotNil(t, hints[0].PaddingLeft)
			assert.False(t, *hints[0].PaddingLeft, "client padding would push the label off the column")

			lines := strings.Split(tt.content, "\n")
			hintLine := int(hints[0].Position.Line)
			lines[hintLine] += labelText(t, hints[0])
			written := strings.Join(lines, "\n")

			journal, errs := parser.Parse(written)
			require.Empty(t, errs)
			opts := formatterOptionsFrom(settings.Formatting)
			for _, edit := range formatter.FormatDocumentWithOptions(journal, written, nil, opts) {
				if int(edit.Range.Start.Line) == hintLine {
					assert.Equal(t, lines[hintLine], edit.NewText)
				}
			}
		})
	}
}

func TestInlayHint_AlignmentSettingsMoveTheHint(t *testing.T) {
	content := "2024-01-15 shop\n    expenses:food  10 USD\n    assets:cash\n"

	srv := NewServer()
	settings := srv.getSettings()
	settings.Formatting.AmountAlignmentMode = "left"
	settings.Formatting.AmountAlignmentColumn = 30
	settings.InlayHints.InferredAmountAlignment = inferredAlignFormatter
	srv.setSettings(settings)
	hints := requestInlayHints(t, srv, content)
	require.Len(t, hints, 1)
	assert.Equal(t, strings.Repeat(" ", 30-15)+"-10 USD", labelText(t, hints[0]))

	settings.Formatting.AlignAmounts = false
	srv.setSettings(settings)
	hints = requestInlayHints(t, srv, content)
	require.Len(t, hints, 1)
	assert.Equal(t, "  -10 USD", labelText(t, hints[0]))
}

func TestInlayHint_TrailingPaddingCountsTowardTheColumn(t *testing.T) {
	line := "    assets:cash    "
	hints := requestInlayHints(t, NewServer(), "2024-01-15 shop\n    expenses:food  $10\n"+line+"\n")
	require.Len(t, hints, 1)
	assert.Equal(t, "-10 $", labelText(t, hints[0]), "the line already reaches the amount column")
	assert.Equal(t, uint32(len(line)), hints[0].Position.Character)
	require.NotNil(t, hints[0].PaddingLeft)
	assert.False(t, *hints[0].PaddingLeft)
}

func TestInlayHint_FallsBackToClientPaddingWhenColumnIsOutOfReach(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{"line already past the column", "    assets:cash                  "},
		{"comment after the account", "    assets:cash  ; a note"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hints := requestInlayHints(t, NewServer(), "2024-01-15 shop\n    expenses:food  $10\n"+tt.line+"\n")
			require.Len(t, hints, 1)
			assert.Equal(t, "-10 $", labelText(t, hints[0]))
			require.NotNil(t, hints[0].PaddingLeft)
			assert.True(t, *hints[0].PaddingLeft)
		})
	}
}

func TestInlayHint_AlignmentWithCRLFDocument(t *testing.T) {
	hints := requestInlayHints(t, NewServer(), "2024-01-15 shop\r\n    expenses:food  $10\r\n    assets:cash\r\n")
	require.Len(t, hints, 1)
	assert.Equal(t, "    -10 $", labelText(t, hints[0]))
}

// decimalColumn returns the display column just past the integer part of the
// number at or after start.
func decimalColumn(t *testing.T, text string, start int, mark string) int {
	t.Helper()
	rest := text[start:]
	digits := strings.IndexAny(rest, "0123456789")
	require.GreaterOrEqual(t, digits, 0, "no number in %q", rest)
	end := digits
	for end < len(rest) && (rest[end] >= '0' && rest[end] <= '9' || rest[end] == ',' || rest[end] == '.') {
		end++
	}
	if dot := strings.LastIndex(rest[:end], mark); dot >= 0 {
		end = dot
	}
	return formatter.DisplayWidth(text[:start+end])
}

// These journals are laid out as hledger-fmt leaves them: decimal points line up
// inside each transaction, and nothing lines up across the file.
func TestInlayHint_TransactionAlignmentMatchesWrittenDecimalPoint(t *testing.T) {
	tests := []struct {
		name    string
		content string
		mark    string
		line    int
	}{
		{"cents", "2024-01-15 shop\n    expenses:food           12.50 USD\n    assets:cash\n", ".", 2},
		{"whole amount against cents", "2024-01-15 shop\n    expenses:food     12 USD\n    fees               0.50 USD\n    assets:cash\n", ".", 3},
		{"left commodity", "2024-01-15 shop\n    expenses:food       $12.50\n    assets:cash\n", ".", 2},
		{"negative left commodity", "2024-01-15 shop\n    assets:bank        -$12.50\n    expenses:food\n", ".", 2},
		{"far from the formatter column", "2024-01-15 long\n    expenses:a:very:long:account:name:indeed  1.00 USD\n    assets:b  -1.00 USD\n\n2024-01-16 shop\n    expenses:c      3.25 USD\n    assets:cash\n", ".", 6},
		{"cyrillic", "2024-01-15 покупка\n    расходы:еда          12.50 ₽\n    активы:наличные\n", ".", 2},
		{"cjk width", "2024-01-15 買い物\n    費用:食品            12.50 円\n    資産:現金\n", ".", 2},
		{"comma mark", "decimal-mark ,\ncommodity 1.000,00 EUR\n\n2024-01-15 shop\n    expenses:food     1.250,50 EUR\n    assets:cash\n", ",", 5},
		{"crlf", "2024-01-15 shop\r\n    expenses:food           12.50 USD\r\n    assets:cash\r\n", ".", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hints := requestInlayHints(t, NewServer(), tt.content)
			require.Len(t, hints, 1)
			assert.Equal(t, uint32(tt.line), hints[0].Position.Line)
			require.NotNil(t, hints[0].PaddingLeft)
			assert.False(t, *hints[0].PaddingLeft)

			lines := strings.Split(strings.ReplaceAll(tt.content, "\r\n", "\n"), "\n")
			written := lines[tt.line-1]
			rendered := lines[tt.line] + labelText(t, hints[0])
			assert.Equal(t,
				decimalColumn(t, written, strings.LastIndex(written, "  ")+2, tt.mark),
				decimalColumn(t, rendered, len(lines[tt.line]), tt.mark),
				"written %q, rendered %q", written, rendered)
		})
	}
}

func TestInlayHint_TransactionAlignmentKeepsTwoSpaceGap(t *testing.T) {
	line := "    assets:with:a:very:long:name:here"
	hints := requestInlayHints(t, NewServer(), "2024-01-15 shop\n    a  1.50 USD\n"+line+"\n")
	require.Len(t, hints, 1)
	assert.Equal(t, "  -1.5 USD", labelText(t, hints[0]))
}

func TestTransactionDecimalColumn_NoWrittenAmount(t *testing.T) {
	content := "2024-01-15 tx\n    assets:a  = 5 USD\n    assets:b\n"
	journal, errs := parser.Parse(content)
	require.Empty(t, errs)

	_, ok := transactionDecimalColumn(strings.Split(content, "\n"), &journal.Transactions[0])
	assert.False(t, ok)
}

func TestInlayHint_AlignmentOptionSelectsTheColumn(t *testing.T) {
	content := "2024-01-15 long\n    expenses:a:very:long:account:name:indeed  1.00 USD\n    assets:b  -1.00 USD\n\n" +
		"2024-01-16 shop\n    expenses:c   3.25 USD\n    assets:cash\n"
	srv := NewServer()
	transaction := labelText(t, requestInlayHints(t, srv, content)[0])

	settings := srv.getSettings()
	settings.InlayHints.InferredAmountAlignment = inferredAlignFormatter
	srv.setSettings(settings)
	assert.Greater(t, len(labelText(t, requestInlayHints(t, srv, content)[0])), len(transaction))
}

// A transaction whose only amounts are hints, the amount an assignment moves and
// the elided amount that balances it, has no written amount to line up with. The
// elided amount lines up with the assignment's hint instead, whichever of the
// two postings is written first, and whatever column the formatter would pick.
func TestInlayHint_ElidedAmountLinesUpWithTheAssignment(t *testing.T) {
	assignment := "    資産:fidelity                   = 3,597.34 USD"
	elided := "    капитал:нереализованное"
	// An elided account longer than the assignment's leaves the negative amount no
	// room to the left, so the assignment's hint moves right to meet it, and the
	// sign spills into the gap after the account.
	tight := "    資産:fidelity  = 3,597.34 USD"
	longElided := "    капитал:нереализованное:долгосрочное"
	for order, postings := range map[string][2]string{
		"assignment first":             {assignment, elided},
		"elided first":                 {elided, assignment},
		"long elided account":          {tight, longElided},
		"long elided account above it": {longElided, tight},
	} {
		for name, newline := range map[string]string{"lf": "\n", "crlf": "\r\n"} {
			t.Run(order+"/"+name, func(t *testing.T) {
				lines := []string{
					"commodity 1,000.00 USD",
					"",
					"2026-01-01 seed",
					"    資産:fidelity  3,570.28 USD",
					"    капитал:взнос  -3,570.28 USD",
					"",
					"2026-03-31 Q1 Statement",
					postings[0],
					postings[1],
					"",
				}
				hints := requestInlayHints(t, NewServer(), strings.Join(lines, newline))

				// The display column of the decimal mark of each hint on lines 7 and 8.
				decimals := make(map[uint32]int)
				for _, hint := range hints {
					if hint.Position.Line != 7 && hint.Position.Line != 8 {
						continue
					}
					line := lines[hint.Position.Line]
					before := line[:lsputil.UTF16OffsetToByteOffset(line, int(hint.Position.Character))]
					label := labelText(t, hint)
					start := formatter.DisplayWidth(before)
					if hint.PaddingLeft != nil && *hint.PaddingLeft {
						start++
					}
					decimals[hint.Position.Line] = start + strings.Index(label, ".")
				}
				require.Len(t, decimals, 2, "%v", inlayLabelsOf(hints))
				assert.Equal(t, decimals[7], decimals[8], "%q", inlayLabelsOf(hints))
			})
		}
	}
}
