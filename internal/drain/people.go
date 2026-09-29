package drain

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/pages"
)

// Person is one row of the resolve's people.json: the speaker labelled
// role, named name, is the wiki person page person (a slug); person is ""
// when the name is ambiguous, with the pages it could be in candidates.
type Person struct {
	Role       string   `json:"role"`
	Name       string   `json:"name"`
	Person     string   `json:"person"`
	Wiki       string   `json:"wiki"`
	Page       string   `json:"page"`
	Created    bool     `json:"created"`
	Candidates []string `json:"candidates,omitempty"`
}

// PeopleTry is a hand-named speaker the resolve did not settle: the name
// it was given, and why. An ambiguous name waits until the page renames
// the speaker; a failed one is tried again after PeopleRetry.
type PeopleTry struct {
	Name    string    `json:"name"`
	Outcome string    `json:"outcome"` // ambiguous | unresolved | failed
	At      time.Time `json:"at"`
}

// placeholder is the name a speaker has before anyone names it.
var placeholder = regexp.MustCompile(`^Speaker \d+$`)

// handNamed is the record's speakers someone named by hand and no person
// page holds yet, role → name: an entry with a role, a name that is not a
// placeholder and not its role, and no person.
func handNamed(raw map[string]any) map[string]string {
	sp, _ := raw["speakers"].([]any)
	var out map[string]string
	for _, x := range sp {
		m, _ := x.(map[string]any)
		role, _ := m["role"].(string)
		name, _ := m["name"].(string)
		person, _ := m["person"].(string)
		name = strings.TrimSpace(name)
		if role == "" || name == "" || person != "" || name == role || placeholder.MatchString(name) {
			continue
		}
		if _, ok := out[role]; ok {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[role] = name
	}
	return out
}

func peopleKey(id, role string) string { return id + "/" + role }

// people resolves every live, unclaimed record's hand-named speakers to wiki
// person pages and writes each person onto its speaker entry; an ingested
// record gets its reingest queued. tries carries the unsettled names from
// tick to tick, so an ambiguous one is not resolved again every tick; the
// returned map replaces it.
func (d *Drain) people(ctx context.Context, tries map[string]PeopleTry) map[string]PeopleTry {
	objs, err := d.Reg.List(ctx, "rec.")
	if err != nil {
		d.logf("people: %v", err)
		return tries
	}
	now := d.Now()
	keep := map[string]PeopleTry{}
	for _, o := range objs {
		if ctx.Err() != nil {
			// Stopped: the records not reached keep their tries.
			for k, t := range tries {
				if _, ok := keep[k]; !ok {
					keep[k] = t
				}
			}
			return keep
		}
		raw, v, err := decode(o)
		if err != nil || v.State == "deleted" {
			continue
		}
		todo := handNamed(raw)
		for role, name := range todo {
			t, ok := tries[peopleKey(v.ID, role)]
			if !ok || t.Name != name {
				continue // a new name, or renamed since the last try
			}
			if t.Outcome != "failed" || now.Sub(t.At) < d.PeopleRetry {
				keep[peopleKey(v.ID, role)] = t
				delete(todo, role)
			}
		}
		if len(todo) == 0 {
			continue
		}
		if v.Claim != nil && v.Claim.Expires.After(now) {
			continue // the record's job is running; the next tick resolves it
		}
		rows, err := d.Stages.Resolve(ctx, v.ID, raw, todo)
		if err != nil {
			if ctx.Err() != nil {
				continue
			}
			for role, name := range todo {
				keep[peopleKey(v.ID, role)] = PeopleTry{Name: name, Outcome: "failed", At: now}
			}
			d.notify(fmt.Sprintf("megameet: %s: the named speakers were not resolved to people: %s (%s)", title(v), oneLine(err.Error()), d.By))
			continue
		}
		found := map[string]Person{}
		for _, r := range rows {
			if name, ok := todo[r.Role]; ok && strings.TrimSpace(r.Name) == name {
				found[r.Role] = r
			}
		}
		set := map[string]Person{}
		for role, name := range todo {
			r, ok := found[role]
			switch {
			case ok && r.Person != "":
				set[role] = Person{Role: role, Name: name, Person: r.Person}
			case ok:
				keep[peopleKey(v.ID, role)] = PeopleTry{Name: name, Outcome: "ambiguous", At: now}
				d.logf("rec.%s: speaker %s «%s» is ambiguous: %s; waits for a rename", v.ID, role, name, strings.Join(r.Candidates, ", "))
			default:
				keep[peopleKey(v.ID, role)] = PeopleTry{Name: name, Outcome: "unresolved", At: now}
				d.logf("rec.%s: speaker %s «%s» is not in the resolve's people.json; waits for a rename", v.ID, role, name)
			}
		}
		if len(set) == 0 {
			continue
		}
		if err := d.setPeople(ctx, v.ID, set); err != nil {
			d.logf("rec.%s: people: %v", v.ID, err)
		}
	}
	return keep
}

var errBusy = fmt.Errorf("claimed meanwhile")

// setPeople writes each resolved person onto the speaker entries with its
// role and the name it was resolved from — only the person key, by
// compare-and-swap on a fresh read, so a page write in between is kept. An
// ingested record gets action reingest and its queue key, as the page's
// Re-ingest button does. A record claimed meanwhile is left for the next
// tick, which resolves it again.
func (d *Drain) setPeople(ctx context.Context, id string, set map[string]Person) error {
	key := "rec." + id
	for try := 0; ; try++ {
		o, err := d.Reg.Record(ctx, key)
		if err != nil {
			return err
		}
		raw, v, err := decode(o)
		if err != nil {
			return err
		}
		if v.State == "deleted" {
			return nil
		}
		if v.Claim != nil && v.Claim.Expires.After(d.Now()) {
			return errBusy
		}
		sp, _ := raw["speakers"].([]any)
		var wrote []string
		for _, x := range sp {
			m, _ := x.(map[string]any)
			role, _ := m["role"].(string)
			name, _ := m["name"].(string)
			person, _ := m["person"].(string)
			if p, ok := set[role]; ok && person == "" && strings.TrimSpace(name) == p.Name {
				m["person"] = p.Person
				wrote = append(wrote, fmt.Sprintf("%s «%s» → %s", role, p.Name, p.Person))
			}
		}
		if len(wrote) == 0 {
			return nil // renamed or named meanwhile
		}
		queue := false
		if v.State == "ingested" {
			switch v.Action {
			case "":
				raw["action"], queue = "reingest", true
			case "reingest":
				queue = true
			default:
				d.logf("rec.%s: %s is queued: the reingest for its people waits for a Re-ingest", id, v.Action)
			}
		}
		raw["updated"] = d.Now().UTC().Format(time.RFC3339Nano)
		_, err = d.Reg.CAS(ctx, key, recSchema, raw, o.Version)
		if pages.Code(err) == "version_conflict" && try < 5 {
			continue
		}
		if err != nil {
			return err
		}
		d.logf("rec.%s: people: %s%s", id, strings.Join(wrote, ", "), map[bool]string{true: "; reingest queued"}[queue])
		if queue {
			if _, err := d.Reg.CAS(ctx, "q."+id, queueSchema, map[string]string{"state": v.State}, 0); err != nil && pages.Code(err) != "version_conflict" {
				return fmt.Errorf("q.%s: %w", id, err)
			}
		}
		return nil
	}
}
