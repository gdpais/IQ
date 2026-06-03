package teams

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"incidentiq/apps/api/internal/config"
	"incidentiq/apps/api/internal/incident"
)

const graphBaseURL = "https://graph.microsoft.com/v1.0"

type Client struct {
	cfg  config.TeamsConfig
	http *http.Client
}

type Status struct {
	Name          string `json:"name"`
	Enabled       bool   `json:"enabled"`
	Configured    bool   `json:"configured"`
	Connected     bool   `json:"connected"`
	SenderDisplay string `json:"sender_display_name,omitempty"`
	SenderUPN     string `json:"sender_upn,omitempty"`
	TenantID      string `json:"tenant_id,omitempty"`
}

type ConnectRequest struct {
	AccessToken  string     `json:"access_token"`
	RefreshToken string     `json:"refresh_token"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	Scopes       string     `json:"scopes,omitempty"`
}

type TestResult struct {
	Name       string `json:"name"`
	Success    bool   `json:"success"`
	StatusCode int    `json:"status_code,omitempty"`
	Message    string `json:"message"`
}

type DirectoryEntry struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Description string `json:"description,omitempty"`
	UPN         string `json:"upn,omitempty"`
}

type ChannelMessageResult struct {
	ID string `json:"id"`
}

type Mention struct {
	ID          int    `json:"id"`
	MentionText string `json:"mentionText"`
	Type        string `json:"type"`
	ObjectID    string `json:"object_id"`
	DisplayName string `json:"display_name"`
}

func NewClient(cfg config.TeamsConfig, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{cfg: cfg, http: httpClient}
}

func (c *Client) Status(state *incident.TeamsAuthState) Status {
	status := Status{
		Name:       incident.TeamsIntegrationName,
		Enabled:    c.cfg.Enabled,
		Configured: c.configured(),
		TenantID:   c.cfg.TenantID,
	}
	if state != nil {
		status.Connected = strings.TrimSpace(state.AccessToken) != "" || strings.TrimSpace(state.RefreshToken) != ""
		status.SenderDisplay = state.SenderDisplay
		status.SenderUPN = state.SenderUPN
	}
	return status
}

func (c *Client) configured() bool {
	return strings.TrimSpace(c.cfg.TenantID) != "" &&
		strings.TrimSpace(c.cfg.ClientID) != "" &&
		strings.TrimSpace(c.cfg.ClientSecret) != "" &&
		strings.TrimSpace(c.cfg.TokenEncryptionKey) != ""
}

func (c *Client) Encrypt(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	block, err := aes.NewCipher(c.encryptionKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	data := gcm.Seal(nonce, nonce, []byte(value), nil)
	return base64.StdEncoding.EncodeToString(data), nil
}

func (c *Client) Decrypt(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(c.encryptionKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce := raw[:gcm.NonceSize()]
	plain, err := gcm.Open(nil, nonce, raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (c *Client) Connect(ctx context.Context, req ConnectRequest) (incident.UpsertTeamsAuthStateRequest, error) {
	if !c.cfg.Enabled {
		return incident.UpsertTeamsAuthStateRequest{}, fmt.Errorf("Teams integration is disabled")
	}
	profile, err := c.profile(ctx, req.AccessToken)
	if err != nil {
		return incident.UpsertTeamsAuthStateRequest{}, err
	}
	accessToken, err := c.Encrypt(req.AccessToken)
	if err != nil {
		return incident.UpsertTeamsAuthStateRequest{}, err
	}
	refreshToken, err := c.Encrypt(req.RefreshToken)
	if err != nil {
		return incident.UpsertTeamsAuthStateRequest{}, err
	}
	return incident.UpsertTeamsAuthStateRequest{
		SenderDisplay: profile.DisplayName,
		SenderUPN:     profile.UserPrincipalName,
		AccessToken:   accessToken,
		RefreshToken:  refreshToken,
		Scopes:        req.Scopes,
		ExpiresAt:     req.ExpiresAt,
	}, nil
}

func (c *Client) Test(ctx context.Context, state *incident.TeamsAuthState) (TestResult, error) {
	accessToken, refreshedState, err := c.accessToken(ctx, state)
	if err != nil {
		return TestResult{Name: incident.TeamsIntegrationName, Message: err.Error()}, err
	}
	_, err = c.profile(ctx, accessToken)
	if err != nil {
		return TestResult{Name: incident.TeamsIntegrationName, Message: err.Error()}, err
	}
	if refreshedState != nil {
		state.AccessToken = refreshedState.AccessToken
		state.RefreshToken = refreshedState.RefreshToken
		state.ExpiresAt = refreshedState.ExpiresAt
	}
	return TestResult{Name: incident.TeamsIntegrationName, Success: true, Message: "Teams connectivity verified"}, nil
}

func (c *Client) ListTeams(ctx context.Context, state *incident.TeamsAuthState) ([]DirectoryEntry, *incident.UpsertTeamsAuthStateRequest, error) {
	var payload struct {
		Value []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
			Description string `json:"description"`
		} `json:"value"`
	}
	auth, updated, err := c.getJSON(ctx, state, graphBaseURL+"/me/joinedTeams", &payload)
	if err != nil {
		return nil, updated, err
	}
	_ = auth
	items := make([]DirectoryEntry, 0, len(payload.Value))
	for _, entry := range payload.Value {
		items = append(items, DirectoryEntry{ID: entry.ID, DisplayName: entry.DisplayName, Description: entry.Description})
	}
	return items, updated, nil
}

func (c *Client) ListChannels(ctx context.Context, state *incident.TeamsAuthState, teamID string) ([]DirectoryEntry, *incident.UpsertTeamsAuthStateRequest, error) {
	var payload struct {
		Value []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
			Description string `json:"description"`
		} `json:"value"`
	}
	_, updated, err := c.getJSON(ctx, state, graphBaseURL+"/teams/"+url.PathEscape(teamID)+"/channels", &payload)
	if err != nil {
		return nil, updated, err
	}
	items := make([]DirectoryEntry, 0, len(payload.Value))
	for _, entry := range payload.Value {
		items = append(items, DirectoryEntry{ID: entry.ID, DisplayName: entry.DisplayName, Description: entry.Description})
	}
	return items, updated, nil
}

func (c *Client) SearchUsers(ctx context.Context, state *incident.TeamsAuthState, query string) ([]DirectoryEntry, *incident.UpsertTeamsAuthStateRequest, error) {
	filter := graphBaseURL + "/users?$top=25&$select=id,displayName,userPrincipalName"
	if strings.TrimSpace(query) != "" {
		filter += "&$filter=" + url.QueryEscape("startswith(displayName,'"+escapeOData(query)+"') or startswith(userPrincipalName,'"+escapeOData(query)+"')")
	}
	var payload struct {
		Value []struct {
			ID                string `json:"id"`
			DisplayName       string `json:"displayName"`
			UserPrincipalName string `json:"userPrincipalName"`
		} `json:"value"`
	}
	_, updated, err := c.getJSON(ctx, state, filter, &payload)
	if err != nil {
		return nil, updated, err
	}
	items := make([]DirectoryEntry, 0, len(payload.Value))
	for _, entry := range payload.Value {
		items = append(items, DirectoryEntry{ID: entry.ID, DisplayName: entry.DisplayName, UPN: entry.UserPrincipalName})
	}
	return items, updated, nil
}

func (c *Client) ListTags(ctx context.Context, state *incident.TeamsAuthState, teamID string) ([]DirectoryEntry, *incident.UpsertTeamsAuthStateRequest, error) {
	var payload struct {
		Value []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
			Description string `json:"description"`
		} `json:"value"`
	}
	_, updated, err := c.getJSON(ctx, state, graphBaseURL+"/teams/"+url.PathEscape(teamID)+"/tags", &payload)
	if err != nil {
		return nil, updated, err
	}
	items := make([]DirectoryEntry, 0, len(payload.Value))
	for _, entry := range payload.Value {
		items = append(items, DirectoryEntry{ID: entry.ID, DisplayName: entry.DisplayName, Description: entry.Description})
	}
	return items, updated, nil
}

func (c *Client) SendChannelMessage(ctx context.Context, state *incident.TeamsAuthState, teamID string, channelID string, body string, mentions []Mention) (ChannelMessageResult, *incident.UpsertTeamsAuthStateRequest, error) {
	accessToken, updated, err := c.accessToken(ctx, state)
	if err != nil {
		return ChannelMessageResult{}, updated, err
	}
	type mentionPayload struct {
		ID        int            `json:"id"`
		MentionText string       `json:"mentionText"`
		Mentioned map[string]any `json:"mentioned"`
	}
	outMentions := make([]mentionPayload, 0, len(mentions))
	for _, mention := range mentions {
		mentioned := map[string]any{
			"displayName": mention.DisplayName,
		}
		switch mention.Type {
		case incident.TeamsRecipientTypeUser:
			mentioned["user"] = map[string]any{"id": mention.ObjectID, "displayName": mention.DisplayName}
		case incident.TeamsRecipientTypeTag:
			mentioned["tag"] = map[string]any{"id": mention.ObjectID, "displayName": mention.DisplayName}
		}
		outMentions = append(outMentions, mentionPayload{
			ID:          mention.ID,
			MentionText: mention.MentionText,
			Mentioned:   mentioned,
		})
	}

	payload := map[string]any{
		"body": map[string]any{
			"contentType": "html",
			"content":     body,
		},
		"mentions": outMentions,
	}
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return ChannelMessageResult{}, updated, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphBaseURL+"/teams/"+url.PathEscape(teamID)+"/channels/"+url.PathEscape(channelID)+"/messages", bytes.NewReader(reqBody))
	if err != nil {
		return ChannelMessageResult{}, updated, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return ChannelMessageResult{}, updated, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ChannelMessageResult{}, updated, graphError(resp.StatusCode, body)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ChannelMessageResult{}, updated, err
	}
	return ChannelMessageResult{ID: out.ID}, updated, nil
}

func BuildPage(detail incident.IncidentDetail, recipients []incident.TeamsRouteRecipient) (string, []Mention) {
	mentions := make([]Mention, 0, len(recipients))
	prefix := make([]string, 0, len(recipients))
	for idx, recipient := range recipients {
		display := strings.TrimSpace(recipient.DisplayName)
		if display == "" {
			continue
		}
		mText := "@"+display
		mentions = append(mentions, Mention{
			ID:          idx,
			MentionText: mText,
			Type:        recipient.Type,
			ObjectID:    recipient.TeamsObjectID,
			DisplayName: display,
		})
		prefix = append(prefix, "<at id=\""+fmt.Sprintf("%d", idx)+"\">"+escapeHTML(display)+"</at>")
	}

	lines := []string{}
	head := strings.Join(prefix, " ")
	state := "incident opened"
	if detail.Incident.Status == incident.StatusOpen {
		for _, event := range detail.Events {
			if event.Type == "incident_reopened" {
				state = "incident reopened"
			}
		}
	}
	if head != "" {
		lines = append(lines, strings.TrimSpace(head+" "+strings.Title(detail.Incident.Severity)+" "+state))
	} else {
		lines = append(lines, strings.Title(detail.Incident.Severity)+" "+state)
	}
	lines = append(lines, "Services: "+escapeHTML(strings.Join(services(detail), ", ")))
	lines = append(lines, "Started: "+detail.Incident.StartedAt.UTC().Format("2006-01-02 15:04 UTC"))
	if issue := issueSummary(detail); issue != "" {
		lines = append(lines, "Issue: "+escapeHTML(issue))
	}
	return strings.Join(lines, "<br/>"), mentions
}

func issueSummary(detail incident.IncidentDetail) string {
	if text := compactText(detail.Incident.Summary); text != "" {
		return truncate(text, 180)
	}
	for _, event := range detail.Events {
		if summary, ok := event.Payload["summary"].(string); ok && compactText(summary) != "" {
			return truncate(compactText(summary), 180)
		}
	}
	return ""
}

func services(detail incident.IncidentDetail) []string {
	seen := map[string]bool{}
	items := []string{}
	add := func(value string) {
		value = compactText(value)
		if value == "" || seen[strings.ToLower(value)] {
			return
		}
		seen[strings.ToLower(value)] = true
		items = append(items, value)
	}
	add(detail.Incident.Service)
	for _, alert := range detail.Alerts {
		add(alert.Service)
	}
	sort.Strings(items)
	return items
}

func compactText(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func truncate(value string, n int) string {
	if len(value) <= n {
		return value
	}
	if n <= 3 {
		return value[:n]
	}
	return strings.TrimSpace(value[:n-3]) + "..."
}

func escapeHTML(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;")
	return replacer.Replace(value)
}

func (c *Client) getJSON(ctx context.Context, state *incident.TeamsAuthState, endpoint string, out any) (string, *incident.UpsertTeamsAuthStateRequest, error) {
	accessToken, updated, err := c.accessToken(ctx, state)
	if err != nil {
		return "", updated, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", updated, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", updated, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", updated, graphError(resp.StatusCode, body)
	}
	return accessToken, updated, json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) accessToken(ctx context.Context, state *incident.TeamsAuthState) (string, *incident.UpsertTeamsAuthStateRequest, error) {
	if !c.configured() {
		return "", nil, fmt.Errorf("Teams integration is not fully configured")
	}
	if state == nil {
		return "", nil, fmt.Errorf("Teams integration is not connected")
	}
	accessToken, err := c.Decrypt(state.AccessToken)
	if err != nil {
		return "", nil, err
	}
	if accessToken != "" && (state.ExpiresAt == nil || time.Until(*state.ExpiresAt) > 2*time.Minute) {
		return accessToken, nil, nil
	}
	refreshToken, err := c.Decrypt(state.RefreshToken)
	if err != nil {
		return "", nil, err
	}
	if refreshToken == "" {
		if accessToken == "" {
			return "", nil, fmt.Errorf("Teams integration is not connected")
		}
		return accessToken, nil, nil
	}
	updated, err := c.refresh(ctx, refreshToken, state)
	if err != nil {
		return "", nil, err
	}
	token, err := c.Decrypt(updated.AccessToken)
	if err != nil {
		return "", nil, err
	}
	return token, &updated, nil
}

func (c *Client) refresh(ctx context.Context, refreshToken string, state *incident.TeamsAuthState) (incident.UpsertTeamsAuthStateRequest, error) {
	values := url.Values{}
	values.Set("grant_type", "refresh_token")
	values.Set("client_id", c.cfg.ClientID)
	values.Set("client_secret", c.cfg.ClientSecret)
	values.Set("refresh_token", refreshToken)
	if strings.TrimSpace(state.Scopes) != "" {
		values.Set("scope", state.Scopes)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://login.microsoftonline.com/"+url.PathEscape(c.cfg.TenantID)+"/oauth2/v2.0/token", strings.NewReader(values.Encode()))
	if err != nil {
		return incident.UpsertTeamsAuthStateRequest{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return incident.UpsertTeamsAuthStateRequest{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return incident.UpsertTeamsAuthStateRequest{}, graphError(resp.StatusCode, body)
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return incident.UpsertTeamsAuthStateRequest{}, err
	}
	encAccess, err := c.Encrypt(payload.AccessToken)
	if err != nil {
		return incident.UpsertTeamsAuthStateRequest{}, err
	}
	nextRefresh := refreshToken
	if payload.RefreshToken != "" {
		nextRefresh = payload.RefreshToken
	}
	encRefresh, err := c.Encrypt(nextRefresh)
	if err != nil {
		return incident.UpsertTeamsAuthStateRequest{}, err
	}
	expiresAt := time.Now().UTC().Add(time.Duration(payload.ExpiresIn) * time.Second)
	return incident.UpsertTeamsAuthStateRequest{
		SenderDisplay: state.SenderDisplay,
		SenderUPN:     state.SenderUPN,
		AccessToken:   encAccess,
		RefreshToken:  encRefresh,
		Scopes:        firstNonEmpty(payload.Scope, state.Scopes),
		ExpiresAt:     &expiresAt,
	}, nil
}

func (c *Client) profile(ctx context.Context, accessToken string) (struct {
	DisplayName       string `json:"displayName"`
	UserPrincipalName string `json:"userPrincipalName"`
}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, graphBaseURL+"/me?$select=displayName,userPrincipalName", nil)
	if err != nil {
		return struct {
			DisplayName       string `json:"displayName"`
			UserPrincipalName string `json:"userPrincipalName"`
		}{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return struct {
			DisplayName       string `json:"displayName"`
			UserPrincipalName string `json:"userPrincipalName"`
		}{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return struct {
			DisplayName       string `json:"displayName"`
			UserPrincipalName string `json:"userPrincipalName"`
		}{}, graphError(resp.StatusCode, body)
	}
	var profile struct {
		DisplayName       string `json:"displayName"`
		UserPrincipalName string `json:"userPrincipalName"`
	}
	if err := json.Unmarshal(body, &profile); err != nil {
		return profile, err
	}
	return profile, nil
}

func graphError(statusCode int, body []byte) error {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && strings.TrimSpace(payload.Error.Message) != "" {
		return fmt.Errorf("Microsoft Graph returned HTTP %d: %s", statusCode, payload.Error.Message)
	}
	return fmt.Errorf("Microsoft Graph returned HTTP %d", statusCode)
}

func escapeOData(value string) string {
	return strings.ReplaceAll(strings.TrimSpace(value), "'", "''")
}

func (c *Client) encryptionKey() []byte {
	sum := sha256.Sum256([]byte(c.cfg.TokenEncryptionKey))
	return sum[:]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
