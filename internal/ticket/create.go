package ticket

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

const (
	// numberAlphabet has 32 characters, which are the letters without I and O and the digits 2 to 9. A byte masked to 5 bits picks
	// one of them with no bias.
	numberAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	suffixLength   = 4
	// mostPerMonth is the largest running number that the shape of a ticket number holds.
	mostPerMonth = 9999
)

// errDiscard ends the transaction of a preview, which does all the work of a creation and then rolls it back.
var errDiscard = errors.New("discard the preview")

// Create makes a ticket for the identity of the session. It checks the fields, then in one transaction it takes the locks of the
// requester, takes a share lock on the models of the request in the order of the id, checks the open tickets and the available
// quantities, makes the number, and writes the ticket, its items and its first history entry. The ticket joins the list that the
// session may read in the same transaction. A request that breaks a rule gives a catalog.ValidationError with every failing field.
func (s *Store) Create(ctx context.Context, req CreateRequest, rec catalog.Recorder) (Ticket, error) {
	return s.create(ctx, req, rec, true)
}

// Preview does everything that Create does and rolls it back, so it stores nothing. It answers a request that filled the hidden
// field of the form, so the bot sees the same answers as a person, the same refusals included, and no ticket exists.
func (s *Store) Preview(ctx context.Context, req CreateRequest) (Ticket, error) {
	return s.create(ctx, req, nil, false)
}

func (s *Store) create(ctx context.Context, req CreateRequest, rec catalog.Recorder, keep bool) (Ticket, error) {
	in, err := req.check(s.limits, s.now())
	if err != nil {
		return Ticket{}, err
	}

	var out Ticket

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockRequester(ctx, tx, req.Identity); err != nil {
			return err
		}

		if err := s.checkStock(ctx, tx, req.Identity, in); err != nil {
			return err
		}

		number, err := s.nextNumber(ctx, tx)
		if err != nil {
			return err
		}

		id, err := s.insert(ctx, tx, req.Identity, in, number)
		if err != nil {
			return err
		}

		if out, err = s.readOne(ctx, tx, `t.id = $1::uuid`, id); err != nil {
			return err
		}

		if !keep {
			return errDiscard
		}

		if _, err := session.AddViewableTicket(ctx, tx, req.SessionHash, id); err != nil {
			return fmt.Errorf("add the ticket to the session %w", err)
		}

		return record(ctx, tx, rec, http.StatusCreated, out)
	})

	if errors.Is(err, errDiscard) {
		return out, nil
	}

	return out, err
}

// lockRequester takes a transaction lock for the email and one for the NIM or NIP, always in this order, so two requests of the same
// person run one after the other and cannot pass the limit of open tickets together. The locks end with the transaction. A request
// holds no other lock of this kind, so the fixed order cannot make two requests wait for each other.
func lockRequester(ctx context.Context, tx pgx.Tx, who session.Identity) error {
	for _, key := range []string{"ticket-email:" + strings.ToLower(who.Email), "ticket-nim:" + strings.ToUpper(who.NimNip)} {
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1::text, 0))`, key); err != nil {
			return fmt.Errorf("lock the requester %w", err)
		}
	}

	return nil
}

// checkStock counts the open tickets of the requester and checks every model of the request, after the share lock on the models. It
// returns every failing field together, in the order of the request.
func (s *Store) checkStock(ctx context.Context, tx pgx.Tx, who session.Identity, in input) error {
	var c checker

	var openCount int

	err := tx.QueryRow(ctx, `
		select count(*)::int from tickets
		where status in (`+sqlList(open)+`) and (lower(requester_email) = $1 or upper(requester_nim_nip) = $2)`,
		strings.ToLower(who.Email), strings.ToUpper(who.NimNip)).Scan(&openCount)
	if err != nil {
		return fmt.Errorf("count the open tickets %w", err)
	}

	if openCount >= s.limits.MaxOpen {
		c.fail("requester", DetailTooManyOpen, "You already have the most open tickets that the lab allows.")
	}

	ids := make([]string, 0, len(in.items))
	for _, it := range in.items {
		ids = append(ids, it.modelID)
	}

	retired, err := catalog.LockModelsShared(ctx, tx, ids...)
	if err != nil {
		return fmt.Errorf("lock the models of the request %w", err)
	}

	available, err := catalog.Available(ctx, tx, ids...)
	if err != nil {
		return fmt.Errorf("read the stock of the request %w", err)
	}

	for i, it := range in.items {
		field := "items[" + strconv.Itoa(i) + "]"

		gone, exists := retired[it.modelID]

		switch {
		case !exists:
			c.fail(field+".modelId", detailUnknownReference, "This model does not exist.")
		case gone:
			c.fail(field+".modelId", DetailModelRetired, "This model is retired.")
		case it.qty > available[it.modelID]:
			c.fail(field+".qty", DetailTooMany, "At most "+strconv.Itoa(available[it.modelID])+" are available.")
		}
	}

	return c.err()
}

// nextNumber allocates the running number of the month and returns the ticket number. The counter row is locked until the
// transaction ends, so two creations never get the same number, and a creation that rolls back gives its number back, so the
// numbers of a month have no gaps.
func (s *Store) nextNumber(ctx context.Context, tx pgx.Tx) (string, error) {
	period := s.now().In(jakarta).Format("0601")

	var last int

	err := tx.QueryRow(ctx, `
		insert into ticket_counters (period, last) values ($1, 1)
		on conflict (period) do update set last = ticket_counters.last + 1
		returning last`, period).Scan(&last)
	if err != nil {
		return "", fmt.Errorf("allocate the number %w", err)
	}

	if last > mostPerMonth {
		return "", ErrNumbersExhausted
	}

	suffix, err := numberSuffix()
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("MGM-%s-%04d-%s", period, last, suffix), nil
}

// numberSuffix draws the last four characters of a number from the random source of the system. They act as the secret of the number.
func numberSuffix() (string, error) {
	raw := make([]byte, suffixLength)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("draw the suffix %w", err)
	}

	for i, b := range raw {
		raw[i] = numberAlphabet[b&(byte(len(numberAlphabet))-1)]
	}

	return string(raw), nil
}

// insert writes the ticket, its items and its first history entry, and returns the id of the ticket.
func (s *Store) insert(ctx context.Context, tx pgx.Tx, who session.Identity, in input, number string) (string, error) {
	at := s.moment()

	var id string

	err := tx.QueryRow(ctx, `
		insert into tickets (number, status, requester_kind, requester_name, requester_nim_nip, requester_email, purpose, pickup_date,
		                     duration_days, created_at)
		values ($1, $2, 'guest', $3, $4, $5, $6, $7::date, $8, $9)
		returning id::text`,
		number, StatusSubmitted, who.Name, who.NimNip, strings.ToLower(who.Email), in.purpose, in.pickup, in.duration, at).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("write the ticket %w", err)
	}

	models := make([]string, 0, len(in.items))
	quantities := make([]int32, 0, len(in.items))

	for _, it := range in.items {
		models = append(models, it.modelID)
		quantities = append(quantities, int32(it.qty)) //nolint:gosec // the sum of the quantities is at most the setting MAX_ITEMS_PER_REQUEST
	}

	_, err = tx.Exec(ctx, `
		insert into ticket_items (ticket_id, model_id, qty)
		select $1::uuid, t.model_id, t.qty from unnest($2::text[]::uuid[], $3::int[]) as t(model_id, qty)`, id, models, quantities)
	if err != nil {
		return "", fmt.Errorf("write the items %w", err)
	}

	_, err = tx.Exec(ctx, `
		insert into ticket_history (ticket_id, at, status, actor_kind) values ($1::uuid, $2, $3, $4)`, id, at, StatusSubmitted, ActorGuest)
	if err != nil {
		return "", fmt.Errorf("write the history %w", err)
	}

	return id, nil
}
