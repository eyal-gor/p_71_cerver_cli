package gateway

import (
	"context"
	"encoding/json"
	"net/url"
)

func remarshal(in any, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// PoolMachine is one machine in a pool, with whether it can take work now.
type PoolMachine struct {
	ComputeID string `json:"compute_id"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	Mine      bool   `json:"mine"`
	Online    bool   `json:"online"`
	Shared    bool   `json:"shared"`
	// State is "free", "busy" or "offline".
	State   string `json:"state"`
	Running int    `json:"running"`
	Max     int    `json:"max"`
	Yours   []struct {
		SessionID string  `json:"session_id"`
		Name      *string `json:"name"`
		Status    string  `json:"status"`
	} `json:"yours"`
	Say string `json:"say"`
}

// Pool is GET /v2/pool: providers, machines with busy/free, people.
type Pool struct {
	PoolID    string `json:"pool_id"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	You       string `json:"you"`
	Say       string `json:"say"`
	InviteURL string `json:"invite_url"`
	Providers []struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
		BilledBy string `json:"billed_by"`
	} `json:"providers"`
	Machines []PoolMachine `json:"machines"`
	People   []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"people"`
	Manage struct {
		Say     string `json:"say"`
		FreeNow []struct {
			ComputeID string `json:"compute_id"`
			Name      string `json:"name"`
		} `json:"free_now"`
	} `json:"manage"`
	PoolsYouAreIn []struct {
		PoolID string `json:"pool_id"`
		Name   string `json:"name"`
	} `json:"pools_you_are_in"`
}

// GetPool reads a pool. poolID "" means your own; state "" means all machines.
func (c *Client) GetPool(ctx context.Context, poolID, state string) (*Pool, map[string]any, error) {
	q := url.Values{}
	if poolID != "" {
		q.Set("pool", poolID)
	}
	if state != "" {
		q.Set("state", state)
	}
	path := "/v2/pool"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var raw map[string]any
	if err := c.Do(ctx, "GET", path, nil, &raw); err != nil {
		return nil, nil, err
	}
	var p Pool
	if err := remarshal(raw, &p); err != nil {
		return nil, nil, err
	}
	// An older gateway sends only online/offline.
	for i := range p.Machines {
		if p.Machines[i].State == "" {
			if p.Machines[i].Online {
				p.Machines[i].State = "free"
			} else {
				p.Machines[i].State = "offline"
			}
		}
	}
	return &p, raw, nil
}

// SharePoolMachine shares (poolID set) or withdraws (poolID "") one of your machines.
func (c *Client) SharePoolMachine(ctx context.Context, computeID, poolID string) error {
	if poolID == "" {
		return c.Do(ctx, "POST", "/v2/computes/"+url.PathEscape(computeID)+"/withdraw", map[string]any{}, nil)
	}
	return c.Do(ctx, "POST", "/v2/computes/"+url.PathEscape(computeID)+"/contribute", map[string]any{"pool_id": poolID}, nil)
}

// NewPoolInvite makes a new invite link. The old link stops working.
func (c *Client) NewPoolInvite(ctx context.Context) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	if err := c.Do(ctx, "POST", "/v2/pool/invite", map[string]any{}, &out); err != nil {
		return "", err
	}
	return out.URL, nil
}
