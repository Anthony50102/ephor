package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Prices are Azure's public pay-as-you-go Linux VM prices, in US dollars an
// hour. They ignore discounts, reservations and disks, so they are a rough
// size of the bill, not the bill.
type Prices struct {
	USD     map[string]float64   `json:"usd"` // by priceKey; -1 means Azure lists no price
	Fetched map[string]time.Time `json:"fetched"`
}

// PriceTTL is how long a fetched price is trusted.
const PriceTTL = 7 * 24 * time.Hour

func priceKey(region, size string, spot bool) string {
	k := strings.ToLower(region) + "|" + strings.ToLower(size)
	if spot {
		k += "|spot"
	}
	return k
}

// LoadPrices reads cached prices, returning an empty set when there are none.
func LoadPrices(path string) *Prices {
	p := &Prices{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, p)
	}
	if p.USD == nil {
		p.USD = map[string]float64{}
	}
	if p.Fetched == nil {
		p.Fetched = map[string]time.Time{}
	}
	return p
}

// Save writes the cache atomically.
func (p *Prices) Save(path string) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Hourly is the price of one VM, and whether it is known.
func (p *Prices) Hourly(region, size string, spot bool) (float64, bool) {
	if p == nil || region == "" || size == "" {
		return 0, false
	}
	v, ok := p.USD[priceKey(region, size, spot)]
	return v, ok && v >= 0
}

// SKU is a VM size in a region, the unit prices are fetched by.
type SKU struct{ Region, Size string }

// Missing lists the sizes rows need that are not cached or are stale.
func (p *Prices) Missing(rows []Row, now time.Time) []SKU {
	seen := map[SKU]bool{}
	var out []SKU
	for _, r := range rows {
		for _, u := range r.units() {
			s := SKU{strings.ToLower(u.region), u.size}
			k := priceKey(s.Region, s.Size, false)
			if seen[s] || u.size == "" || u.region == "" || now.Sub(p.Fetched[k]) < PriceTTL {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Region+out[i].Size < out[j].Region+out[j].Size })
	return out
}

// Merge adds fetched prices. Sizes Azure did not list are remembered as
// unknown so they are not asked about again until the cache expires.
func (p *Prices) Merge(skus []SKU, got map[string]float64, now time.Time) {
	for _, s := range skus {
		for _, spot := range []bool{false, true} {
			k := priceKey(s.Region, s.Size, spot)
			if v, ok := got[k]; ok {
				p.USD[k] = v
			} else {
				p.USD[k] = -1
			}
			p.Fetched[k] = now
		}
	}
}

// Getter fetches a URL.
type Getter func(ctx context.Context, url string) ([]byte, error)

// HTTPGetter fetches over HTTPS. The price list is public and needs no
// sign-in.
func HTTPGetter() Getter {
	c := &http.Client{Timeout: 30 * time.Second}
	return func(ctx context.Context, u string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("price list: %s", resp.Status)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	}
}

const priceAPI = "https://prices.azure.com/api/retail/prices"

// FetchPrices asks the price list about each size, a region and ten sizes
// per request, and returns prices by key.
func FetchPrices(ctx context.Context, get Getter, skus []SKU) (map[string]float64, error) {
	by := map[string][]string{}
	for _, s := range skus {
		if safeName.MatchString(s.Size) && safeName.MatchString(s.Region) {
			by[s.Region] = append(by[s.Region], s.Size)
		}
	}
	out := map[string]float64{}
	for region, sizes := range by {
		for len(sizes) > 0 {
			n := min(10, len(sizes))
			batch := sizes[:n]
			sizes = sizes[n:]
			var or []string
			for _, s := range batch {
				or = append(or, "armSkuName eq '"+s+"'")
			}
			q := url.Values{}
			q.Set("api-version", "2023-01-01-preview")
			q.Set("$filter", "serviceName eq 'Virtual Machines' and priceType eq 'Consumption' and armRegionName eq '"+
				region+"' and ("+strings.Join(or, " or ")+")")
			next := priceAPI + "?" + q.Encode()
			for page := 0; next != "" && page < 10; page++ {
				b, err := get(ctx, next)
				if err != nil {
					return nil, err
				}
				next, err = readPrices(b, region, out)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	return out, nil
}

func readPrices(b []byte, region string, out map[string]float64) (string, error) {
	var page struct {
		Items []struct {
			Size    string  `json:"armSkuName"`
			SKU     string  `json:"skuName"`
			Product string  `json:"productName"`
			Price   float64 `json:"retailPrice"`
			Unit    string  `json:"unitOfMeasure"`
		} `json:"Items"`
		Next string `json:"NextPageLink"`
	}
	if err := json.Unmarshal(b, &page); err != nil {
		return "", fmt.Errorf("unreadable price list: %w", err)
	}
	for _, it := range page.Items {
		if strings.HasSuffix(it.Product, "Windows") || it.Unit != "1 Hour" || strings.HasSuffix(it.SKU, "Low Priority") {
			continue
		}
		out[priceKey(region, it.Size, strings.HasSuffix(it.SKU, " Spot"))] = it.Price
	}
	return page.Next, nil
}

// unit is some number of one VM size that is running.
type unit struct {
	region, size string
	spot         bool
	n            int
}

// units are the VMs a row is paying for right now.
func (r Row) units() []unit {
	l := r.Live
	if l == nil || !r.Billing() {
		return nil
	}
	if l.VMSize != "" {
		return []unit{{l.Location, l.VMSize, l.Spot, 1}}
	}
	var out []unit
	for _, p := range l.Pools {
		if p.Mode != "Machines" && !p.Stopped && p.Count > 0 && p.Size != "" {
			out = append(out, unit{l.Location, p.Size, p.Spot, p.Count})
		}
	}
	for _, m := range l.Machines {
		if m.Running {
			out = append(out, unit{l.Location, m.Size, m.Spot, 1})
		}
	}
	return out
}

// Hourly is what the row's running VMs cost an hour at list price, and
// whether every price was known. Rows that run nothing cost 0.
func (r Row) Hourly(p *Prices) (float64, bool) {
	total, known := 0.0, true
	for _, u := range r.units() {
		v, ok := p.Hourly(u.region, u.size, u.spot)
		if !ok {
			known = false
			continue
		}
		total += v * float64(u.n)
	}
	return total, known
}

// PoolHourly is one node pool's price an hour, when known.
func PoolHourly(p *Prices, region string, pl Pool) (float64, bool) {
	if pl.Stopped || pl.Count == 0 {
		return 0, true
	}
	v, ok := p.Hourly(region, pl.Size, pl.Spot)
	return v * float64(pl.Count), ok
}
