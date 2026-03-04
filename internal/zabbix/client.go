package zabbix

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type AuthMethod string

const (
	AuthToken        AuthMethod = "token"
	AuthUserPassword AuthMethod = "userpass"
)

var ErrNotFound = errors.New("zabbix object not found")

func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}

// FlexInt unmarshals from JSON string or number (Zabbix API may return either).
type FlexInt int

// FlexIntFrom converts an int to FlexInt.
func FlexIntFrom(v int) FlexInt { return FlexInt(v) }

func (v *FlexInt) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return err
		}
		*v = FlexInt(n)
		return nil
	}
	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*v = FlexInt(n)
	return nil
}

type Auth struct {
	Method   AuthMethod
	Token    string
	Username string
	Password string
}

type ClientConfig struct {
	URL             string
	Timeout         time.Duration
	InsecureSkipTLS bool
	Auth            Auth
}

type Client struct {
	url        string
	httpClient *http.Client
	auth       Auth

	mu          sync.Mutex
	sessionAuth string
	rpcID       int64
}

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
	Auth    string      `json:"auth,omitempty"`
	ID      int64       `json:"id"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
	ID      int64           `json:"id"`
}

func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("Zabbix URL is required")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.InsecureSkipTLS}, //nolint:gosec
	}

	return &Client{
		url: cfg.URL,
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
		auth:  cfg.Auth,
		rpcID: 1,
	}, nil
}

func (c *Client) nextID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rpcID++
	return c.rpcID
}

func (c *Client) ensureAuth(ctx context.Context) (string, error) {
	if c.auth.Method == AuthToken {
		if c.auth.Token == "" {
			return "", errors.New("api_token is empty")
		}
		return c.auth.Token, nil
	}

	c.mu.Lock()
	if c.sessionAuth != "" {
		token := c.sessionAuth
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

	params := map[string]any{
		"username": c.auth.Username,
		"password": c.auth.Password,
	}

	var token string
	if err := c.callNoAuth(ctx, "user.login", params, &token); err != nil {
		return "", err
	}

	c.mu.Lock()
	c.sessionAuth = token
	c.mu.Unlock()

	return token, nil
}

func (c *Client) Ping(ctx context.Context) error {
	var version string
	return c.callNoAuth(ctx, "apiinfo.version", map[string]any{}, &version)
}

func (c *Client) callNoAuth(ctx context.Context, method string, params interface{}, out interface{}) error {
	return c.call(ctx, method, params, false, out)
}

func (c *Client) callAuth(ctx context.Context, method string, params interface{}, out interface{}) error {
	return c.call(ctx, method, params, true, out)
}

func (c *Client) call(ctx context.Context, method string, params interface{}, withAuth bool, out interface{}) error {
	if method == "host.create" || method == "host.update" || method == "hostinterface.replacehostinterfaces" ||
		method == "hostinterface.update" || method == "hostinterface.create" || method == "hostinterface.delete" {
		if b, err := json.Marshal(params); err == nil {
			log.Printf("zabbix client debug: %s params=%s", method, string(b))
		}
	}

	requestBody := rpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      c.nextID(),
	}
	if withAuth {
		token, err := c.ensureAuth(ctx)
		if err != nil {
			return err
		}
		requestBody.Auth = token
	}

	rawReq, err := json.Marshal(requestBody)
	if err != nil {
		return err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(rawReq))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json-rpc")

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()

	rawResp, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return err
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		return fmt.Errorf("http status %d: %s", httpResp.StatusCode, string(rawResp))
	}

	var payload rpcResponse
	if err := json.Unmarshal(rawResp, &payload); err != nil {
		return err
	}
	if payload.Error != nil {
		return fmt.Errorf("zabbix api error (%d) %s: %s", payload.Error.Code, payload.Error.Message, payload.Error.Data)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(payload.Result, out)
}

type Tag struct {
	Tag   string `json:"tag"`
	Value string `json:"value"`
}

type SNMPDetails struct {
	// Zabbix 6.4 returns version as a JSON string ("2"), flexInt handles both string and number.
	Version   FlexInt `json:"version,omitempty"`
	Community string  `json:"community,omitempty"`
	Security  string  `json:"securityname,omitempty"`
	AuthProto string  `json:"authprotocol,omitempty"`
	AuthPass  string  `json:"authpassphrase,omitempty"`
	PrivProto string  `json:"privprotocol,omitempty"`
	PrivPass  string  `json:"privpassphrase,omitempty"`
	Context   string  `json:"contextname,omitempty"`
}

// parseInt accepts JSON number or string for Zabbix API compatibility (some versions return main/type/useip as string).
func parseInt(v interface{}) int {
	if v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		i, _ := strconv.Atoi(x)
		return i
	case int:
		return x
	default:
		return 0
	}
}

// parseString accepts JSON string or number for Zabbix API compatibility (e.g. port, status as number).
func parseString(v interface{}) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	default:
		return ""
	}
}

type hostInterfaceJSON struct {
	InterfaceID string          `json:"interfaceid,omitempty"`
	Type        interface{}     `json:"type"`
	Main        interface{}     `json:"main"`
	UseIP       interface{}     `json:"useip"`
	IP          string          `json:"ip,omitempty"`
	DNS         string          `json:"dns,omitempty"`
	Port        interface{}     `json:"port,omitempty"` // API can return number or string
	Details     json.RawMessage `json:"details,omitempty"`
}

func (hi *HostInterface) UnmarshalJSON(data []byte) error {
	var raw hostInterfaceJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	hi.InterfaceID = raw.InterfaceID
	hi.Type = parseInt(raw.Type)
	hi.Main = parseInt(raw.Main)
	hi.UseIP = parseInt(raw.UseIP)
	hi.IP = raw.IP
	hi.DNS = raw.DNS
	hi.Port = parseString(raw.Port)
	hi.Details = parseSNMPDetails(raw.Details)
	return nil
}

// parseSNMPDetails accepts details as JSON object or array (Zabbix API can return either).
func parseSNMPDetails(data json.RawMessage) *SNMPDetails {
	if len(data) == 0 {
		return nil
	}
	var single SNMPDetails
	if err := json.Unmarshal(data, &single); err == nil {
		return &single
	}
	var arr []SNMPDetails
	if err := json.Unmarshal(data, &arr); err == nil && len(arr) > 0 {
		return &arr[0]
	}
	return nil
}

type HostInterface struct {
	InterfaceID string       `json:"interfaceid,omitempty"`
	Type        int          `json:"type"`
	Main        int          `json:"main"`
	UseIP       int          `json:"useip"`
	IP          string       `json:"ip,omitempty"`
	DNS         string       `json:"dns"` // API requires dns present (use "" when using IP)
	Port        string       `json:"port,omitempty"`
	Details     *SNMPDetails `json:"details,omitempty"`
}

type Host struct {
	HostID string `json:"hostid"`
	Host   string `json:"host"`
	Name   string `json:"name"`
	Status string `json:"status"`

	Interfaces []HostInterface `json:"interfaces"`
	Groups     []struct {
		GroupID string `json:"groupid"`
		Name    string `json:"name"`
	} `json:"groups"`
	ParentTemplates []struct {
		TemplateID string `json:"templateid"`
		Host       string `json:"host"`
		Name       string `json:"name"`
	} `json:"parentTemplates"`
	Tags []Tag `json:"tags"`
}

type hostJSON struct {
	HostID   interface{} `json:"hostid"`   // API can return string or number
	Host     string      `json:"host"`
	Name     string      `json:"name"`
	Status   interface{} `json:"status"`  // API can return "0"/"1" or 0/1
	Interfaces []HostInterface `json:"interfaces"`
	Groups   []struct {
		GroupID string `json:"groupid"`
		Name    string `json:"name"`
	} `json:"groups"`
	ParentTemplates []struct {
		TemplateID string `json:"templateid"`
		Host       string `json:"host"`
		Name       string `json:"name"`
	} `json:"parentTemplates"`
	Tags []Tag `json:"tags"`
}

func (h *Host) UnmarshalJSON(data []byte) error {
	var raw hostJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	h.HostID = parseString(raw.HostID)
	h.Host = raw.Host
	h.Name = raw.Name
	h.Status = parseString(raw.Status)
	h.Interfaces = raw.Interfaces
	h.Groups = raw.Groups
	h.ParentTemplates = raw.ParentTemplates
	h.Tags = raw.Tags
	return nil
}

type HostCreateRequest struct {
	Host        string
	Name        string
	Status      int
	Interfaces  []HostInterface
	GroupIDs    []string
	TemplateIDs []string
	Tags        []Tag
}

type HostUpdateRequest = HostCreateRequest

// interfacesForHostCreate builds the interfaces payload for host.create so it matches Zabbix API expectations:
// no interfaceid, dns always set, SNMP details with version/community/bulk.
func interfacesForHostCreate(ifaces []HostInterface) []map[string]any {
	out := make([]map[string]any, 0, len(ifaces))
	for _, i := range ifaces {
		m := map[string]any{
			"type":  i.Type,
			"main":  i.Main,
			"useip": i.UseIP,
			"ip":    i.IP,
			"dns":   i.DNS,
			"port":  i.Port,
		}
		if i.Type == 2 && i.Details != nil {
			m["details"] = map[string]any{
				"version":   int(i.Details.Version),
				"community": i.Details.Community,
				"bulk":      1,
			}
		}
		out = append(out, m)
	}
	return out
}

// interfacesForHostUpdate builds the interfaces payload for host.update: same as create but with
// interfaceid set for existing interfaces (type,main) so the API updates in place and applies details.
func interfacesForHostUpdate(ifaces []HostInterface, currentByTypeMain map[string]string) []map[string]any {
	out := make([]map[string]any, 0, len(ifaces))
	for _, i := range ifaces {
		m := map[string]any{
			"type":  i.Type,
			"main":  i.Main,
			"useip": i.UseIP,
			"ip":    i.IP,
			"dns":   i.DNS,
			"port":  i.Port,
		}
		key := fmt.Sprintf("%d,%d", i.Type, i.Main)
		if id := currentByTypeMain[key]; id != "" {
			m["interfaceid"] = id
		}
		if i.Type == 2 && i.Details != nil {
			m["details"] = map[string]any{
				"version":   int(i.Details.Version),
				"community": i.Details.Community,
				"bulk":      1,
			}
		}
		out = append(out, m)
	}
	return out
}

// allInterfacesAreSNMP returns true if every interface is type 2 (SNMP). host.create fails with
// "Incorrect arguments" when only SNMP interfaces are sent, so we use a workaround.
func allInterfacesAreSNMP(ifaces []HostInterface) bool {
	if len(ifaces) == 0 {
		return false
	}
	for _, i := range ifaces {
		if i.Type != 2 {
			return false
		}
	}
	return true
}

func (c *Client) HostCreate(ctx context.Context, req HostCreateRequest) (string, error) {
	groups := make([]map[string]string, 0, len(req.GroupIDs))
	for _, g := range req.GroupIDs {
		groups = append(groups, map[string]string{"groupid": g})
	}

	templates := make([]map[string]string, 0, len(req.TemplateIDs))
	for _, t := range req.TemplateIDs {
		templates = append(templates, map[string]string{"templateid": t})
	}

	interfacesToSend := req.Interfaces
	skipTemplatesOnCreate := false
	if allInterfacesAreSNMP(req.Interfaces) {
		// Zabbix host.create rejects a single SNMP interface; create with temporary Agent then replace.
		interfacesToSend = []HostInterface{{
			Type:  1,
			Main:  1,
			UseIP: 1,
			IP:    "127.0.0.1",
			DNS:   "",
			Port:  "10050",
		}}
		skipTemplatesOnCreate = true
	}

	params := map[string]any{
		"host":       req.Host,
		"name":       req.Name,
		"status":     req.Status,
		"interfaces": interfacesForHostCreate(interfacesToSend),
		"groups":     groups,
	}
	if len(req.Tags) > 0 {
		params["tags"] = req.Tags
	}
	if len(templates) > 0 && !skipTemplatesOnCreate {
		params["templates"] = templates
	}

	var result struct {
		HostIDs []string `json:"hostids"`
	}
	if err := c.callAuth(ctx, "host.create", params, &result); err != nil {
		return "", err
	}
	if len(result.HostIDs) == 0 {
		return "", errors.New("host.create returned no hostid")
	}
	hostID := result.HostIDs[0]

	if allInterfacesAreSNMP(req.Interfaces) {
		curHost, err := c.HostGetByID(ctx, hostID)
		if err != nil {
			return "", fmt.Errorf("host.get after create: %w", err)
		}
		var ignored any
		for _, iface := range curHost.Interfaces {
			if err := c.callAuth(ctx, "hostinterface.delete", []string{iface.InterfaceID}, &ignored); err != nil {
				return "", fmt.Errorf("hostinterface.delete (temp): %w", err)
			}
		}
		for _, i := range req.Interfaces {
			payload := map[string]any{
				"hostid": hostID,
				"type":   i.Type,
				"main":   i.Main,
				"useip":  i.UseIP,
				"ip":     i.IP,
				"dns":    i.DNS,
				"port":   i.Port,
			}
			if i.Details != nil {
				payload["details"] = map[string]any{
					"version":   int(i.Details.Version),
					"community": i.Details.Community,
					"bulk":      1,
				}
			}
			if err := c.callAuth(ctx, "hostinterface.create", payload, &ignored); err != nil {
				return "", fmt.Errorf("hostinterface.create (SNMP): %w", err)
			}
		}
		if len(templates) > 0 {
			updateParams := map[string]any{"hostid": hostID, "templates": templates}
			if err := c.callAuth(ctx, "host.update", updateParams, &ignored); err != nil {
				return "", fmt.Errorf("host.update (templates): %w", err)
			}
		}
	}

	return hostID, nil
}

func (c *Client) HostGetByID(ctx context.Context, hostID string) (*Host, error) {
	params := map[string]any{
		"hostids":               []string{hostID},
		"output":                []string{"hostid", "host", "name", "status"},
		"selectInterfaces":      "extend",
		"selectGroups":          []string{"groupid", "name"},
		"selectParentTemplates": []string{"templateid", "host", "name"},
		"selectTags":            "extend",
	}

	var hosts []Host
	if err := c.callAuth(ctx, "host.get", params, &hosts); err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return nil, ErrNotFound
	}
	return &hosts[0], nil
}

func (c *Client) HostUpdate(ctx context.Context, hostID string, req HostUpdateRequest) error {
	// Get current host first so we have interfaceids for existing interfaces (type,main).
	curHost, err := c.HostGetByID(ctx, hostID)
	if err != nil {
		return fmt.Errorf("host.get (interfaces): %w", err)
	}
	currentByTypeMain := make(map[string]string)
	for _, iface := range curHost.Interfaces {
		key := fmt.Sprintf("%d,%d", iface.Type, iface.Main)
		currentByTypeMain[key] = iface.InterfaceID
	}

	groups := make([]map[string]string, 0, len(req.GroupIDs))
	for _, g := range req.GroupIDs {
		groups = append(groups, map[string]string{"groupid": g})
	}

	templates := make([]map[string]string, 0, len(req.TemplateIDs))
	for _, t := range req.TemplateIDs {
		templates = append(templates, map[string]string{"templateid": t})
	}

	tags := req.Tags
	if tags == nil {
		tags = []Tag{}
	}
	params := map[string]any{
		"hostid":     hostID,
		"host":       req.Host,
		"status":     strconv.Itoa(req.Status),
		"groups":     groups,
		"tags":       tags,
		"interfaces": interfacesForHostUpdate(req.Interfaces, currentByTypeMain),
	}
	if req.Name != "" {
		params["name"] = req.Name
	}
	if len(templates) > 0 {
		params["templates"] = templates
	}

	var ignored any
	if err := c.callAuth(ctx, "host.update", params, &ignored); err != nil {
		return fmt.Errorf("host.update: %w", err)
	}
	return nil
}

func (c *Client) HostDelete(ctx context.Context, hostID string) error {
	var ignored any
	return c.callAuth(ctx, "host.delete", []string{hostID}, &ignored)
}

type HostGroup struct {
	GroupID string `json:"groupid"`
	Name    string `json:"name"`
}

func (c *Client) HostGroupCreate(ctx context.Context, name string) (string, error) {
	var result struct {
		GroupIDs []string `json:"groupids"`
	}
	if err := c.callAuth(ctx, "hostgroup.create", map[string]string{"name": name}, &result); err != nil {
		return "", err
	}
	if len(result.GroupIDs) == 0 {
		return "", errors.New("hostgroup.create returned no groupid")
	}
	return result.GroupIDs[0], nil
}

func (c *Client) HostGroupGetByID(ctx context.Context, id string) (*HostGroup, error) {
	params := map[string]any{
		"groupids": []string{id},
		"output":   []string{"groupid", "name"},
	}
	var groups []HostGroup
	if err := c.callAuth(ctx, "hostgroup.get", params, &groups); err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, ErrNotFound
	}
	return &groups[0], nil
}

func (c *Client) HostGroupIDsByNames(ctx context.Context, names []string) ([]string, error) {
	out := make([]string, 0, len(names))
	for _, name := range names {
		params := map[string]any{
			"output": []string{"groupid", "name"},
			"filter": map[string]any{
				"name": []string{name},
			},
		}
		var groups []HostGroup
		if err := c.callAuth(ctx, "hostgroup.get", params, &groups); err != nil {
			return nil, err
		}
		if len(groups) == 0 {
			return nil, fmt.Errorf("host group not found: %s", name)
		}
		if len(groups) > 1 {
			return nil, fmt.Errorf("ambiguous host group: %s", name)
		}
		out = append(out, groups[0].GroupID)
	}
	return out, nil
}

func (c *Client) HostGroupUpdate(ctx context.Context, id, name string) error {
	var ignored any
	return c.callAuth(ctx, "hostgroup.update", map[string]string{"groupid": id, "name": name}, &ignored)
}

func (c *Client) HostGroupDelete(ctx context.Context, id string) error {
	var ignored any
	return c.callAuth(ctx, "hostgroup.delete", []string{id}, &ignored)
}

type Template struct {
	TemplateID string `json:"templateid"`
	Host       string `json:"host"`
	Name       string `json:"name"`
	Groups     []struct {
		GroupID string `json:"groupid"`
	} `json:"groups"`
	Macros []struct {
		Macro string `json:"macro"`
		Value string `json:"value"`
	} `json:"macros"`
}

func (c *Client) TemplateCreate(ctx context.Context, host, name string, groupIDs []string, macros map[string]string) (string, error) {
	groups := make([]map[string]string, 0, len(groupIDs))
	for _, g := range groupIDs {
		groups = append(groups, map[string]string{"groupid": g})
	}
	params := map[string]any{
		"host":   host,
		"name":   name,
		"groups": groups,
	}
	if len(macros) > 0 {
		macrosArr := make([]map[string]string, 0, len(macros))
		for k, v := range macros {
			macrosArr = append(macrosArr, map[string]string{"macro": k, "value": v})
		}
		params["macros"] = macrosArr
	}
	var result struct {
		TemplateIDs []string `json:"templateids"`
	}
	if err := c.callAuth(ctx, "template.create", params, &result); err != nil {
		return "", err
	}
	if len(result.TemplateIDs) == 0 {
		return "", errors.New("template.create returned no templateid")
	}
	return result.TemplateIDs[0], nil
}

func (c *Client) TemplateGetByID(ctx context.Context, id string) (*Template, error) {
	params := map[string]any{
		"templateids":   []string{id},
		"output":       []string{"templateid", "host", "name"},
		"selectGroups": []string{"groupid"},
		"selectMacros":  "extend",
	}
	var templates []Template
	if err := c.callAuth(ctx, "template.get", params, &templates); err != nil {
		return nil, err
	}
	if len(templates) == 0 {
		return nil, ErrNotFound
	}
	return &templates[0], nil
}

func (c *Client) TemplateIDsByNames(ctx context.Context, names []string) ([]string, error) {
	out := make([]string, 0, len(names))
	for _, name := range names {
		id, err := c.templateIDByName(ctx, name)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

func (c *Client) templateIDByName(ctx context.Context, name string) (string, error) {
	// Priorite au nom interne "host" (cas le plus frequent dans Zabbix).
	paramsByHost := map[string]any{
		"output": []string{"templateid", "host", "name"},
		"filter": map[string]any{
			"host": []string{name},
		},
	}
	var hostMatches []Template
	if err := c.callAuth(ctx, "template.get", paramsByHost, &hostMatches); err != nil {
		return "", err
	}
	if len(hostMatches) == 1 {
		return hostMatches[0].TemplateID, nil
	}
	if len(hostMatches) > 1 {
		return "", fmt.Errorf("ambiguous template (host): %s", name)
	}

	paramsByName := map[string]any{
		"output": []string{"templateid", "host", "name"},
		"filter": map[string]any{
			"name": []string{name},
		},
	}
	var nameMatches []Template
	if err := c.callAuth(ctx, "template.get", paramsByName, &nameMatches); err != nil {
		return "", err
	}
	if len(nameMatches) == 0 {
		return "", fmt.Errorf("template not found: %s", name)
	}
	if len(nameMatches) > 1 {
		return "", fmt.Errorf("ambiguous template (name): %s", name)
	}
	return nameMatches[0].TemplateID, nil
}

func (c *Client) TemplateUpdate(ctx context.Context, id, host, name string, groupIDs []string, macros map[string]string) error {
	groups := make([]map[string]string, 0, len(groupIDs))
	for _, g := range groupIDs {
		groups = append(groups, map[string]string{"groupid": g})
	}
	params := map[string]any{
		"templateid": id,
		"host":       host,
		"name":       name,
		"groups":     groups,
	}
	if len(macros) > 0 {
		macrosArr := make([]map[string]string, 0, len(macros))
		for k, v := range macros {
			macrosArr = append(macrosArr, map[string]string{"macro": k, "value": v})
		}
		params["macros"] = macrosArr
	}
	var ignored any
	return c.callAuth(ctx, "template.update", params, &ignored)
}

func (c *Client) TemplateDelete(ctx context.Context, id string) error {
	var ignored any
	return c.callAuth(ctx, "template.delete", []string{id}, &ignored)
}

type Trigger struct {
	TriggerID   string `json:"triggerid"`
	Description string `json:"description"`
	Expression  string `json:"expression"`
	Priority    string `json:"priority"`
	Status      string `json:"status"`
}

func (c *Client) TriggerCreate(ctx context.Context, description, expression, priority string, enabled bool) (string, error) {
	params := map[string]any{
		"description": description,
		"expression":  expression,
		"priority":    priority,
		"status":      boolToStatus(enabled),
	}
	var result struct {
		TriggerIDs []string `json:"triggerids"`
	}
	if err := c.callAuth(ctx, "trigger.create", params, &result); err != nil {
		return "", err
	}
	if len(result.TriggerIDs) == 0 {
		return "", errors.New("trigger.create returned no triggerid")
	}
	return result.TriggerIDs[0], nil
}

func (c *Client) TriggerGetByID(ctx context.Context, id string) (*Trigger, error) {
	params := map[string]any{
		"triggerids":        []string{id},
		"output":            []string{"triggerid", "description", "expression", "priority", "status"},
		"expandExpression": true, // return last(/Host/key) instead of {itemid} to avoid config drift
	}
	var triggers []Trigger
	if err := c.callAuth(ctx, "trigger.get", params, &triggers); err != nil {
		return nil, err
	}
	if len(triggers) == 0 {
		return nil, ErrNotFound
	}
	return &triggers[0], nil
}

func (c *Client) TriggerUpdate(ctx context.Context, id, description, expression, priority string, enabled bool) error {
	params := map[string]any{
		"triggerid":   id,
		"description": description,
		"expression":  expression,
		"priority":    priority,
		"status":      boolToStatus(enabled),
	}
	var ignored any
	return c.callAuth(ctx, "trigger.update", params, &ignored)
}

func (c *Client) TriggerDelete(ctx context.Context, id string) error {
	var ignored any
	return c.callAuth(ctx, "trigger.delete", []string{id}, &ignored)
}

// Item: 0=Zabbix agent, 1=SNMPv1, 2=SNMPv2c, 3=SNMPv3...
// ValueType : 0=float, 1=str, 2=log, 3=unsigned, 4=text
type Item struct {
	ItemID    string  `json:"itemid"`
	HostID    string  `json:"hostid"`
	Name      string  `json:"name"`
	Key       string  `json:"key_"`
	Type      FlexInt `json:"type"`
	ValueType FlexInt `json:"value_type"`
	SNMPOid   string  `json:"snmp_oid"`
	Units     string  `json:"units"`
	Delay     string  `json:"delay"`
	History   string  `json:"history"`
	Trends    string  `json:"trends"`
	DelayFlex string  `json:"delay_flex"`
	Status    string  `json:"status"` // 0=enabled, 1=disabled
}

type ItemCreateRequest struct {
	HostID    string
	Name      string
	Key       string
	Type      int    // 2 = SNMPv2 agent
	ValueType int    // 3 = unsigned
	SNMPOid   string
	Units     string
	Delay     string // ex: "10m", "60s"
	History   string // ex: "90d"
	Trends    string // ex: "365d"
	DelayFlex string // ex: "50s;1-7,00:00-24:00"
	Enabled   bool
}

// Host interface type constants (Zabbix API).
const (
	HostInterfaceAgent = 1
	HostInterfaceSNMP  = 2
	HostInterfaceIPMI  = 3
	HostInterfaceJMX   = 4
)


const (
	ItemTypeZabbixAgent = 0
	ItemTypeTrapper     = 2
	ItemTypeSNMPAgent   = 20
)

func (c *Client) getSNMPInterfaceIDForItem(ctx context.Context, hostID string) (string, error) {
	host, err := c.HostGetByID(ctx, hostID)
	if err != nil {
		if IsNotFound(err) {
			// It's a template (templates are not returned by host.get) - no interface needed.
			return "0", nil
		}
		return "", err
	}
	for _, iface := range host.Interfaces {
		if iface.Type == HostInterfaceSNMP {
			return iface.InterfaceID, nil
		}
	}
	// Host exists but has no SNMP interface - use "0".
	return "0", nil
}

func (c *Client) ItemCreate(ctx context.Context, req ItemCreateRequest) (string, error) {
	delayParam := any(req.Delay)
	if req.Delay == "0" {
		delayParam = 0
	}
	params := map[string]any{
		"hostid":     req.HostID,
		"name":       req.Name,
		"key_":       req.Key,
		"type":       req.Type,
		"value_type": req.ValueType,
		"delay":      delayParam,
		"history":   req.History,
		"trends":    req.Trends,
		"status":    strconv.Itoa(boolToStatus(req.Enabled)),
	}
	if req.Units != "" {
		params["units"] = req.Units
	}
	// SNMP agent (type 20 in Zabbix 6.4+): send snmp_oid and interfaceid.
	// interfaceid is "0" for templates (no interface), or the real SNMP interface ID for hosts.
	if req.Type == ItemTypeSNMPAgent {
		snmpOID := strings.TrimSpace(req.SNMPOid)
		if snmpOID == "" {
			return "", errors.New("snmp_oid is required for SNMP agent items (type 20)")
		}
		params["snmp_oid"] = snmpOID
		interfaceID, err := c.getSNMPInterfaceIDForItem(ctx, req.HostID)
		if err != nil {
			return "", fmt.Errorf("could not resolve SNMP interface: %w", err)
		}
		params["interfaceid"] = interfaceID
	}
	var result struct {
		ItemIDs []string `json:"itemids"`
	}
	if err := c.callAuth(ctx, "item.create", params, &result); err != nil {
		return "", err
	}
	if len(result.ItemIDs) == 0 {
		return "", errors.New("item.create returned no itemid")
	}
	return result.ItemIDs[0], nil
}

func (c *Client) ItemGetByID(ctx context.Context, id string) (*Item, error) {
	params := map[string]any{
		"itemids": []string{id},
		"output":  []string{"itemid", "hostid", "name", "key_", "type", "value_type", "snmp_oid", "units", "delay", "history", "trends", "delay_flex", "status"},
	}
	var items []Item
	if err := c.callAuth(ctx, "item.get", params, &items); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrNotFound
	}
	return &items[0], nil
}

func (c *Client) ItemUpdate(ctx context.Context, itemID string, req ItemCreateRequest) error {
	delayParam := any(req.Delay)
	if req.Delay == "0" {
		delayParam = 0
	}
	params := map[string]any{
		"itemid":     itemID,
		"name":       req.Name,
		"key_":       req.Key,
		"type":       req.Type,
		"value_type": req.ValueType,
		"delay":      delayParam,
		"history":    req.History,
		"trends":     req.Trends,
		"status":    strconv.Itoa(boolToStatus(req.Enabled)),
	}
	if req.Units != "" {
		params["units"] = req.Units
	}
	// SNMP agent (type 20 in Zabbix 6.4+): send snmp_oid and interfaceid.
	if req.Type == ItemTypeSNMPAgent {
		snmpOID := strings.TrimSpace(req.SNMPOid)
		if snmpOID == "" {
			return errors.New("snmp_oid is required for SNMP agent items (type 20)")
		}
		params["snmp_oid"] = snmpOID
		interfaceID, err := c.getSNMPInterfaceIDForItem(ctx, req.HostID)
		if err != nil {
			return fmt.Errorf("could not resolve SNMP interface: %w", err)
		}
		params["interfaceid"] = interfaceID
	}
	var ignored any
	return c.callAuth(ctx, "item.update", params, &ignored)
}

func (c *Client) ItemDelete(ctx context.Context, id string) error {
	var ignored any
	return c.callAuth(ctx, "item.delete", []string{id}, &ignored)
}

func StatusToEnabled(status string) bool {
	value, err := strconv.Atoi(status)
	if err != nil {
		return false
	}
	return value == 0
}

func boolToStatus(enabled bool) int {
	if enabled {
		return 0
	}
	return 1
}

// --- Action (trigger notifications, e.g. send email) ---

// flexString unmarshals JSON number or string into string (API may return conditiontype/operator as number).
type flexString string

func (s *flexString) UnmarshalJSON(data []byte) error {
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		*s = flexString(strconv.Itoa(n))
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	*s = flexString(str)
	return nil
}

// ActionCondition for 6.4 (top-level) or 7.x (inside filter).
type ActionCondition struct {
	ConditionType flexString `json:"conditiontype"`
	Operator      flexString `json:"operator"`
	Value         string     `json:"value"`
}

// Action object (trigger-based, eventsource=0). Zabbix 7.x returns filter.conditions, 6.4 returns conditions at root.
type Action struct {
	ActionID     string `json:"actionid"`
	Name         string `json:"name"`
	EventSource  string `json:"eventsource"` // "0" = trigger
	EvalType     string `json:"evaltype"`
	Status       string `json:"status"` // "0"=enabled, "1"=disabled
	EscPeriod    string `json:"esc_period"`
	DefShortData string `json:"def_shortdata"`
	DefLongData  string `json:"def_longdata"`
	Conditions   []ActionCondition `json:"conditions,omitempty"` // 6.4
	Filter       *struct {
		Conditions []ActionCondition `json:"conditions"`
		EvalType   string            `json:"evaltype"`
		Formula    string            `json:"formula"`
	} `json:"filter,omitempty"` // 7.x
	Operations []struct {
		OperationType string `json:"operationtype"`
		OpmessageGrp  []struct {
			UsrgrpID string `json:"usrgrpid"`
		} `json:"opmessage_grp,omitempty"`
		Opmessage *struct {
			DefaultMsg string `json:"default_msg"`
		} `json:"opmessage,omitempty"`
	} `json:"operations"`
}

// ActionCreateRequest for trigger-based action (send message to user groups).
type ActionCreateRequest struct {
	Name              string
	UserGroupIDs      []string // usrgrpid list
	Subject           string   // def_shortdata
	Message           string   // def_longdata
	Enabled           bool
	EscPeriod         string   // e.g. "1h", "60s"
	HostGroupIDs      []string // optional: restrict to hosts in these host groups (conditiontype 0)
	TriggerNameLike   []string // optional: restrict to triggers whose name contains any of these
}

func (c *Client) ActionCreate(ctx context.Context, req ActionCreateRequest) (string, error) {
	// API 7.x style: "filter" (not "conditions" at root). conditiontype 0 = Host group, 3 = trigger description.
	conditions := make([]map[string]any, 0)
	for _, gid := range req.HostGroupIDs {
		if gid == "" {
			continue
		}
		conditions = append(conditions, map[string]any{"conditiontype": 0, "operator": 0, "value": gid})
	}
	for _, pat := range req.TriggerNameLike {
		if pat == "" {
			continue
		}
		conditions = append(conditions, map[string]any{"conditiontype": 3, "operator": 2, "value": pat})
	}
	if len(conditions) == 0 {
		conditions = []map[string]any{{"conditiontype": 3, "operator": 2, "value": ""}}
	}
	evaltype := 1   // AND: host in group AND (name like X OR name like Y)
	if len(conditions) > 1 {
		evaltype = 2 // OR between conditions (e.g. group Lampe OR group Laser OR trigger name)
	}
	opmessageGrp := make([]map[string]string, 0, len(req.UserGroupIDs))
	for _, gid := range req.UserGroupIDs {
		opmessageGrp = append(opmessageGrp, map[string]string{"usrgrpid": gid})
	}
	operations := []map[string]any{
		{
			"operationtype":  "0",
			"opmessage_grp":  opmessageGrp,
			"opmessage":      map[string]string{"default_msg": "1"},
		},
	}
	status := "1"
	if req.Enabled {
		status = "0"
	}
	escPeriod := req.EscPeriod
	if escPeriod == "" {
		escPeriod = "1h"
	}
	filter := map[string]any{"conditions": conditions, "evaltype": evaltype}
	params := map[string]any{
		"name":        req.Name,
		"eventsource": "0",
		"filter":      filter,
		"status":      status,
		"esc_period":  escPeriod,
		"operations":  operations,
	}
	// Note: this API version rejects def_shortdata/def_longdata and opmessage subject/message; message uses media type default.
	var result struct {
		ActionIDs []string `json:"actionids"`
	}
	if err := c.callAuth(ctx, "action.create", params, &result); err != nil {
		return "", err
	}
	if len(result.ActionIDs) == 0 {
		return "", errors.New("action.create returned no actionid")
	}
	return result.ActionIDs[0], nil
}

func (c *Client) ActionGetByID(ctx context.Context, id string) (*Action, error) {
	params := map[string]any{
		"actionids":        []string{id},
		"output":           "extend",
		"selectFilter":     "extend",
		"selectConditions": "extend",
		"selectOperations": "extend",
	}
	var actions []Action
	if err := c.callAuth(ctx, "action.get", params, &actions); err != nil {
		return nil, err
	}
	if len(actions) == 0 {
		return nil, ErrNotFound
	}
	return &actions[0], nil
}

func (c *Client) ActionUpdate(ctx context.Context, id string, req ActionCreateRequest) error {
	conditions := make([]map[string]any, 0)
	for _, gid := range req.HostGroupIDs {
		if gid == "" {
			continue
		}
		conditions = append(conditions, map[string]any{"conditiontype": 0, "operator": 0, "value": gid})
	}
	for _, pat := range req.TriggerNameLike {
		if pat == "" {
			continue
		}
		conditions = append(conditions, map[string]any{"conditiontype": 3, "operator": 2, "value": pat})
	}
	if len(conditions) == 0 {
		conditions = []map[string]any{{"conditiontype": 3, "operator": 2, "value": ""}}
	}
	evaltype := 1
	if len(conditions) > 1 {
		evaltype = 2
	}
	opmessageGrp := make([]map[string]string, 0, len(req.UserGroupIDs))
	for _, gid := range req.UserGroupIDs {
		opmessageGrp = append(opmessageGrp, map[string]string{"usrgrpid": gid})
	}
	operations := []map[string]any{
		{
			"operationtype": "0",
			"opmessage_grp": opmessageGrp,
			"opmessage":     map[string]string{"default_msg": "1"},
		},
	}
	status := "1"
	if req.Enabled {
		status = "0"
	}
	escPeriod := req.EscPeriod
	if escPeriod == "" {
		escPeriod = "1h"
	}
	filter := map[string]any{"conditions": conditions, "evaltype": evaltype}
	params := map[string]any{
		"actionid":   id,
		"name":       req.Name,
		"filter":     filter,
		"status":     status,
		"esc_period": escPeriod,
		"operations": operations,
	}
	var ignored any
	return c.callAuth(ctx, "action.update", params, &ignored)
}

func (c *Client) ActionDelete(ctx context.Context, id string) error {
	var ignored any
	return c.callAuth(ctx, "action.delete", []string{id}, &ignored)
}

// --- User group (for action recipients) ---

type UserGroup struct {
	UsrgrpID string `json:"usrgrpid"`
	Name     string `json:"name"`
}

func (c *Client) UserGroupGetByID(ctx context.Context, id string) (*UserGroup, error) {
	params := map[string]any{
		"usrgrpids": []string{id},
		"output":    []string{"usrgrpid", "name"},
	}
	var groups []UserGroup
	if err := c.callAuth(ctx, "usergroup.get", params, &groups); err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, ErrNotFound
	}
	return &groups[0], nil
}

// UserGroupIDsByNames returns usergroup IDs for the given names (e.g. "Zabbix administrators").
func (c *Client) UserGroupIDsByNames(ctx context.Context, names []string) ([]string, error) {
	out := make([]string, 0, len(names))
	for _, name := range names {
		params := map[string]any{
			"output": []string{"usrgrpid", "name"},
			"filter": map[string]any{"name": []string{name}},
		}
		var groups []UserGroup
		if err := c.callAuth(ctx, "usergroup.get", params, &groups); err != nil {
			return nil, err
		}
		if len(groups) == 0 {
			return nil, fmt.Errorf("user group not found: %s", name)
		}
		if len(groups) > 1 {
			return nil, fmt.Errorf("ambiguous user group: %s", name)
		}
		out = append(out, groups[0].UsrgrpID)
	}
	return out, nil
}

func (c *Client) UserGroupCreate(ctx context.Context, name string) (string, error) {
	params := map[string]any{"name": name}
	var result struct {
		UsrgrpIDs []string `json:"usrgrpids"`
	}
	if err := c.callAuth(ctx, "usergroup.create", params, &result); err != nil {
		return "", err
	}
	if len(result.UsrgrpIDs) == 0 {
		return "", errors.New("usergroup.create returned no usrgrpid")
	}
	return result.UsrgrpIDs[0], nil
}

func (c *Client) UserGroupUpdate(ctx context.Context, id, name string) error {
	var ignored any
	return c.callAuth(ctx, "usergroup.update", map[string]any{"usrgrpid": id, "name": name}, &ignored)
}

func (c *Client) UserGroupDelete(ctx context.Context, id string) error {
	var ignored any
	return c.callAuth(ctx, "usergroup.delete", []string{id}, &ignored)
}

// UserCreateRequest for creating a Zabbix user (e.g. for notifications).
type UserCreateRequest struct {
	Username   string
	Name       string // display name
	Password   string
	UserGrpIDs []string // must contain at least one group
	RoleID     string   // "1"=User, "2"=Admin, "3"=Super admin
	Email      string   // if set, add Email media (mediatypeid 1) with this sendto
}

func (c *Client) UserCreate(ctx context.Context, req UserCreateRequest) (string, error) {
	usrgrps := make([]map[string]string, 0, len(req.UserGrpIDs))
	for _, gid := range req.UserGrpIDs {
		usrgrps = append(usrgrps, map[string]string{"usrgrpid": gid})
	}
	params := map[string]any{
		"username": req.Username,
		"name":     req.Name,
		"usrgrps":  usrgrps,
		"roleid":   req.RoleID,
	}
	if req.RoleID == "" {
		params["roleid"] = "1"
	}
	if req.Password != "" {
		params["passwd"] = req.Password
	}
	if req.Email != "" {
		params["medias"] = []map[string]any{
			{
				"mediatypeid": "1",
				"sendto":      []string{req.Email},
				"active":      0,
				"severity":    63,
				"period":      "1-7,00:00-24:00",
			},
		}
	}
	var result struct {
		UserIDs []string `json:"userids"`
	}
	if err := c.callAuth(ctx, "user.create", params, &result); err != nil {
		return "", err
	}
	if len(result.UserIDs) == 0 {
		return "", errors.New("user.create returned no userid")
	}
	return result.UserIDs[0], nil
}

func (c *Client) UserGetByID(ctx context.Context, id string) (*User, error) {
	params := map[string]any{
		"userids":        []string{id},
		"output":         "extend",
		"selectMedias":   "extend",
		"selectUsrgrps":  "extend",
	}
	var users []User
	if err := c.callAuth(ctx, "user.get", params, &users); err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, ErrNotFound
	}
	return &users[0], nil
}

type sendToString string

func (s *sendToString) UnmarshalJSON(data []byte) error {
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		if len(arr) > 0 {
			*s = sendToString(arr[0])
		}
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	*s = sendToString(str)
	return nil
}

type User struct {
	UserID   string `json:"userid"`
	Username string `json:"username"`
	Name     string `json:"name"`
	RoleID   string `json:"roleid"`
	Medias   []struct {
		MediaTypeID string       `json:"mediatypeid"`
		SendTo      sendToString `json:"sendto"`
	} `json:"medias,omitempty"`
	Usrgrps []struct {
		UsrgrpID string `json:"usrgrpid"`
	} `json:"usrgrps,omitempty"`
}

func (c *Client) UserUpdate(ctx context.Context, userID string, req UserCreateRequest) error {
	usrgrps := make([]map[string]string, 0, len(req.UserGrpIDs))
	for _, gid := range req.UserGrpIDs {
		usrgrps = append(usrgrps, map[string]string{"usrgrpid": gid})
	}
	params := map[string]any{
		"userid":   userID,
		"username": req.Username,
		"name":     req.Name,
		"usrgrps":  usrgrps,
		"roleid":   req.RoleID,
	}
	if req.RoleID == "" {
		params["roleid"] = "1"
	}
	if req.Password != "" {
		params["passwd"] = req.Password
	}
	if req.Email != "" {
		params["medias"] = []map[string]any{
			{
				"mediatypeid": "1",
				"sendto":      []string{req.Email},
				"active":      0,
				"severity":    63,
				"period":      "1-7,00:00-24:00",
			},
		}
	}
	var ignored any
	return c.callAuth(ctx, "user.update", params, &ignored)
}

func (c *Client) UserDelete(ctx context.Context, id string) error {
	var ignored any
	return c.callAuth(ctx, "user.delete", []string{id}, &ignored)
}

