package panel

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxOperatorRoutes = 12

type OperatorRoute struct {
	Operator, Transport, Endpoint string
}

func (r *Registry) loadOperatorRoutes(ctx context.Context) error {
	if _, err := r.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS operator_routes (
		position INTEGER PRIMARY KEY, operator TEXT NOT NULL, transport TEXT NOT NULL, endpoint TEXT NOT NULL
	)`); err != nil {
		return err
	}
	rows, err := r.db.QueryContext(ctx, "SELECT operator, transport, endpoint FROM operator_routes ORDER BY position")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var route OperatorRoute
		if err := rows.Scan(&route.Operator, &route.Transport, &route.Endpoint); err != nil {
			return err
		}
		r.operatorRoutes = append(r.operatorRoutes, route)
	}
	return rows.Err()
}

func (r *Registry) OperatorRoutes() []OperatorRoute {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.operatorRoutes)
}

func (r *Registry) saveOperatorRoutes(ctx context.Context, routes []OperatorRoute) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM operator_routes"); err != nil {
		return err
	}
	for position, route := range routes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO operator_routes (position, operator, transport, endpoint)
			VALUES ($1, $2, $3, $4)`, position, route.Operator, route.Transport, route.Endpoint); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.mu.Lock()
	r.operatorRoutes = slices.Clone(routes)
	r.mu.Unlock()
	return nil
}

func validateOperatorRoutes(routes []OperatorRoute, cfg Config) ([]OperatorRoute, error) {
	var valid []OperatorRoute
	names, addresses := make(map[string]bool), make(map[string]bool)
	for i, route := range routes {
		route.Operator = strings.TrimSpace(route.Operator)
		route.Endpoint = strings.TrimSpace(route.Endpoint)
		if route.Operator == "" && route.Endpoint == "" {
			continue
		}
		if route.Operator == "" || utf8.RuneCountInString(route.Operator) > 40 || strings.IndexFunc(route.Operator, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("Enter an operator name of 1–40 characters in row %d.", i+1)
		}
		label := strings.ToLower(route.Operator)
		if names[label] {
			return nil, fmt.Errorf("The operator name in row %d is already in use.", i+1)
		}
		names[label] = true

		routeKey := ""
		switch route.Transport {
		case "reality":
			address := route.Endpoint
			if address == "" && cfg.Reality != nil {
				address = cfg.Reality.Public
			}
			if address != "" {
				host, port, err := splitEndpoint(address)
				if err != nil || (net.ParseIP(host) == nil && normalizedHostname(host) == "") {
					return nil, fmt.Errorf("Enter a REALITY hostname or IP and port in row %d.", i+1)
				}
				routeKey = net.JoinHostPort(strings.ToLower(host), strconv.Itoa(port))
				if route.Endpoint != "" {
					route.Endpoint = routeKey
				}
			}
		case "ws":
			base := cfg.ProxyURL
			if route.Endpoint != "" {
				parsed, err := origin(route.Endpoint)
				if err != nil || parsed.Scheme != "https" {
					return nil, fmt.Errorf("Enter a WebSocket HTTPS origin in row %d.", i+1)
				}
				base = parsed
				route.Endpoint = parsed.String()
			}
			routeKey = base.Scheme + "://" + strings.ToLower(profileAddress(base))
		default:
			return nil, fmt.Errorf("Choose a valid transport in row %d.", i+1)
		}
		key := route.Transport + "|" + routeKey
		if addresses[key] {
			return nil, fmt.Errorf("Row %d uses the same transport and address as another route.", i+1)
		}
		addresses[key] = true
		valid = append(valid, route)
		if len(valid) > maxOperatorRoutes {
			return nil, fmt.Errorf("Add up to %d operator routes.", maxOperatorRoutes)
		}
	}
	return valid, nil
}

func subscriptionProfiles(reg *Registry, base *url.URL, reality *Reality, u User) []string {
	var profiles []string
	for _, route := range reg.OperatorRoutes() {
		name := u.Remark + " (" + route.Operator + ")"
		switch route.Transport {
		case "reality":
			if reality != nil {
				address := route.Endpoint
				if address == "" {
					address = reality.Public
				}
				profiles = append(profiles, reality.profileAt(address, u.ID, name))
			}
		case "ws":
			endpoint := base
			if route.Endpoint != "" {
				var err error
				endpoint, err = origin(route.Endpoint)
				if err != nil {
					continue
				}
			}
			profiles = append(profiles, profile(endpoint, u, PrimaryPath, name))
		}
	}
	if len(profiles) == 0 {
		primary := profile(base, u, PrimaryPath, u.Remark)
		if reality != nil {
			primary = reality.profileAt(reality.Public, u.ID, u.Remark)
		}
		profiles = append(profiles, primary)
	}
	return append(profiles, profile(base, u, BalancePath, u.Username+"-"+FormatBytes(u.Remaining())))
}
