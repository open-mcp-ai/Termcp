package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const localDaemonCredentialVersion = 1

type localDaemonCredential struct {
	Version        int    `json:"version"`
	RegistrationID string `json:"registration_id"`
	AgentID        string `json:"agent_id"`
	TokenID        string `json:"token_id"`
	Token          string `json:"token"`
}

// LocalDaemonCredentialPath is the trusted local bridge handoff. The file is
// intentionally outside encrypted auth JSON because the bridge must read it
// before it can authenticate. It is owner-only and never served over HTTP.
func LocalDaemonCredentialPath(dataDir string) string {
	return filepath.Join(dataDir, "local-daemon-credential.json")
}

func readLocalDaemonCredential(dataDir string) (localDaemonCredential, error) {
	raw, err := os.ReadFile(LocalDaemonCredentialPath(dataDir))
	if err != nil {
		return localDaemonCredential{}, err
	}
	defer clear(raw)
	if len(raw) > 16<<10 {
		return localDaemonCredential{}, errors.New("local daemon credential file is too large")
	}
	var credential localDaemonCredential
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&credential); err != nil {
		return localDaemonCredential{}, errors.New("invalid local daemon credential file")
	}
	if credential.Version != localDaemonCredentialVersion ||
		credential.RegistrationID == "" || credential.AgentID == "" ||
		credential.TokenID == "" || !strings.HasPrefix(credential.Token, "tmcp_") {
		return localDaemonCredential{}, errors.New("invalid local daemon credential file")
	}
	return credential, nil
}

// ReadLocalDaemonCredential returns the local bridge credential for this data
// directory. It performs no network or auth-state access.
func ReadLocalDaemonCredential(dataDir string) (string, error) {
	credential, err := readLocalDaemonCredential(dataDir)
	if err != nil {
		return "", err
	}
	return credential.Token, nil
}

func writeLocalDaemonCredential(dataDir string, credential localDaemonCredential) error {
	raw, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	defer clear(raw)
	target := LocalDaemonCredentialPath(dataDir)
	temp, err := os.CreateTemp(dataDir, ".local-daemon-credential-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return replaceLocalDaemonCredential(tempName, target)
}

// EnsureLocalDaemonRegistration creates or repairs the single daemon-managed
// local Agent registration. It is called by the local server, never by an
// unauthenticated HTTP route. A revoked registration is a durable tombstone:
// deleting or revoking the Agent/token does not resurrect it on restart.
func (m *Managed) EnsureLocalDaemonRegistration(ctx context.Context) (bool, error) {
	if m == nil {
		return true, errors.New("managed auth is unavailable")
	}
	dataDir := filepath.Dir(m.path)
	fileCredential, fileErr := readLocalDaemonCredential(dataDir)

	var provisioned localDaemonCredential
	var removeFile bool
	var replacedCredentialID string
	err := m.commitState(ctx, func(next *managedState) (func(), error) {
		registration := next.DaemonRegistration
		if registration != nil && registration.RevokedAt != nil {
			removeFile = true
			return nil, nil
		}
		if registration != nil {
			agent, agentOK := next.Agents[registration.AgentID]
			token, tokenOK := next.Tokens[registration.TokenID]
			if agentOK && agent.Origin != agentOriginDaemon {
				return nil, errors.New("daemon registration points to a non-daemon Agent")
			}
			if tokenOK && token.Kind != tokenKindDaemon {
				return nil, errors.New("daemon registration points to a non-daemon credential")
			}
			if !agentOK {
				now := m.now().UTC()
				registration.RevokedAt = &now
				registration.RevokeReason = "agent_missing"
				next.DaemonRegistration = registration
				removeFile = true
				return nil, nil
			}
			if !agent.Enabled {
				// A disabled Agent is an identity suspension, not a request
				// to re-register. Keep the file/credential so an explicit
				// re-enable can resume it, while every request remains denied.
				return nil, nil
			}
			if tokenOK && tokenLive(token, m.now().UTC()) && fileErr == nil &&
				fileCredential.RegistrationID == registration.ID &&
				fileCredential.AgentID == registration.AgentID &&
				fileCredential.TokenID == registration.TokenID &&
				equalSecret(fileCredential.Token, strings.TrimSpace(fileCredential.Token)) &&
				token.Digest == tokenDigest(fileCredential.Token) {
				return nil, nil
			}
			if tokenOK && token.Kind == tokenKindDaemon && token.RevokedAt != nil {
				now := m.now().UTC()
				registration.RevokedAt = &now
				registration.RevokeReason = "credential_revoked"
				next.DaemonRegistration = registration
				removeFile = true
				return nil, nil
			}
			replacedCredentialID = registration.TokenID
			tokens, replacement, mintErr := m.replaceDaemonCredential(next, registration)
			if mintErr != nil {
				return nil, mintErr
			}
			next.Tokens, provisioned = tokens, replacement
			registration.TokenID = provisioned.TokenID
			registration.ProvisionedAt = m.now().UTC()
			next.DaemonRegistration = registration
			return func() {
				if replacedCredentialID == "" {
					return
				}
				m.mu.Lock()
				m.cancelCredentialLocked(replacedCredentialID)
				m.mu.Unlock()
			}, nil
		}

		groupID := next.DefaultGroupID
		group, ok := next.Groups[groupID]
		if !ok || !group.Enabled {
			return nil, errors.New("default permission group is unavailable for daemon registration")
		}
		if len(next.Agents) >= maxAgents {
			return nil, errors.New("Agent limit reached for daemon registration")
		}
		agentID, err := randomSecret()
		if err != nil {
			return nil, err
		}
		now := m.now().UTC()
		agent := agentRecord{
			ID: agentID, Name: "Local stdio bridge", GroupID: groupID,
			Enabled: true, Revision: 1, CreatedAt: now, Origin: agentOriginDaemon,
		}
		next.Agents[agent.ID] = agent
		registration = &daemonRegistration{
			ID: localDaemonRegistrationID, AgentID: agent.ID,
			CreatedAt: now, ProvisionedAt: now,
		}
		tokens, replacement, mintErr := m.replaceDaemonCredential(next, registration)
		if mintErr != nil {
			return nil, mintErr
		}
		next.Tokens, provisioned = tokens, replacement
		registration.TokenID = provisioned.TokenID
		next.DaemonRegistration = registration
		return nil, nil
	})
	if err != nil {
		return false, err
	}
	if removeFile {
		fileErr := os.Remove(LocalDaemonCredentialPath(dataDir))
		if errors.Is(fileErr, os.ErrNotExist) {
			fileErr = nil
		}
		if fileErr != nil {
			return false, fileErr
		}
	}
	if provisioned.Token != "" {
		if err := writeLocalDaemonCredential(dataDir, provisioned); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (m *Managed) replaceDaemonCredential(next *managedState, registration *daemonRegistration) (map[string]tokenRecord, localDaemonCredential, error) {
	now := m.now().UTC()
	if registration.TokenID != "" {
		if old, ok := next.Tokens[registration.TokenID]; ok && tokenActive(old) {
			old.RevokedAt = &now
			next.Tokens[old.ID] = old
		}
	}
	token, raw, err := m.mintToken(next, registration.AgentID, "Local stdio bridge", nil)
	if err != nil {
		return nil, localDaemonCredential{}, err
	}
	token.Kind = tokenKindDaemon
	next.Tokens[token.ID] = token
	return next.Tokens, localDaemonCredential{
		Version: localDaemonCredentialVersion, RegistrationID: registration.ID,
		AgentID: registration.AgentID, TokenID: token.ID, Token: raw,
	}, nil
}
