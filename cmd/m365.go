package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/opencode-ai/opencode/internal/config"
	"github.com/opencode-ai/opencode/internal/llm/models"
	"github.com/opencode-ai/opencode/internal/llm/provider"
	"github.com/opencode-ai/opencode/internal/llm/tools"
	"github.com/opencode-ai/opencode/internal/m365auth"
	"github.com/opencode-ai/opencode/internal/message"
	"github.com/spf13/cobra"
)

var m365Cmd = &cobra.Command{
	Use:   "m365",
	Short: "Sign in to and check the Microsoft 365 Copilot provider",
	Long: `Manage the Microsoft 365 Copilot provider, which talks to Microsoft 365 Copilot
through the Copilot Chat API in Microsoft Graph.

The Chat API requires a Microsoft 365 Copilot license and a work or school account.`,
	Example: `
  # Sign in with your browser
  opencode m365 login

  # Sign in from a machine without a browser (e.g. over SSH)
  opencode m365 login --device-code

  # Check the sign-in and send a test prompt to Copilot
  opencode m365 status --test`,
}

var m365LoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Sign in to Microsoft 365",
	RunE: func(cmd *cobra.Command, args []string) error {
		settings := m365Settings(cmd)
		deviceCode, _ := cmd.Flags().GetBool("device-code")

		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		// Longer than a device code lives (15 minutes), so an expired code is reported as such.
		ctx, cancelTimeout := context.WithTimeout(ctx, 20*time.Minute)
		defer cancelTimeout()

		client := &http.Client{Timeout: 60 * time.Second}
		var (
			tok *m365auth.CachedToken
			err error
		)
		if deviceCode {
			tok, err = m365auth.LoginWithDeviceCode(ctx, client, settings, func(dc m365auth.DeviceCode) {
				fmt.Println(dc.Message)
			})
		} else {
			tok, err = m365auth.LoginWithBrowser(ctx, client, settings, func(url string) error {
				fmt.Println("Opening your browser to sign in to Microsoft 365...")
				fmt.Printf("If it doesn't open, visit:\n\n  %s\n\n", url)
				if err := m365auth.OpenBrowser(url); err != nil {
					fmt.Fprintf(os.Stderr, "Couldn't open a browser (%v). Open the link above, or use --device-code.\n", err)
				}
				return nil
			})
		}
		if err != nil {
			return fmt.Errorf("sign-in failed: %w", err)
		}
		if err := m365auth.SaveCachedToken(tok); err != nil {
			return err
		}

		account := tok.Username
		if account == "" {
			account = "your account"
		}
		fmt.Printf("Signed in to Microsoft 365 as %s.\n", account)
		if missing := missingScopes(tok.Scope); len(missing) > 0 {
			fmt.Printf("Warning: the token is missing permissions the Copilot Chat API needs: %s.\n"+
				"Ask an administrator to grant consent for them.\n", strings.Join(missing, ", "))
		}
		fmt.Println("Run `opencode m365 status --test` to check that Copilot answers, then `opencode` to start.")
		return nil
	},
}

var m365LogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Remove the cached Microsoft 365 sign-in",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := m365auth.DeleteCachedToken(); err != nil {
			return err
		}
		fmt.Println("Signed out of Microsoft 365.")
		return nil
	},
}

var m365StatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the Microsoft 365 sign-in and optionally send a test prompt",
	RunE: func(cmd *cobra.Command, args []string) error {
		test, _ := cmd.Flags().GetBool("test")
		loadConfigQuietly(cmd)

		if os.Getenv(m365auth.AccessTokenEnv) != "" {
			fmt.Printf("Using the access token from %s.\n", m365auth.AccessTokenEnv)
		} else {
			tok, err := m365auth.LoadCachedToken()
			if errors.Is(err, os.ErrNotExist) {
				fmt.Println("Not signed in. Run `opencode m365 login`.")
				return nil
			}
			if err != nil {
				return err
			}
			fmt.Printf("Signed in as:   %s\n", valueOr(tok.Username, "(unknown)"))
			fmt.Printf("Tenant:         %s\n", tok.TenantID)
			fmt.Printf("Client ID:      %s\n", tok.ClientID)
			if time.Now().Before(tok.ExpiresAt) {
				fmt.Printf("Access token:   valid until %s\n", tok.ExpiresAt.Local().Format(time.RFC1123))
			} else {
				fmt.Println("Access token:   expired (it is refreshed automatically)")
			}
			if missing := missingScopes(tok.Scope); len(missing) > 0 {
				fmt.Printf("Missing scopes: %s\n", strings.Join(missing, ", "))
			}
			fmt.Printf("Cache file:     %s\n", m365auth.TokenPath())
		}
		if !test {
			return nil
		}

		fmt.Println("\nSending a test prompt to Microsoft 365 Copilot...")
		p, err := provider.NewProvider(models.ProviderM365Copilot,
			provider.WithModel(models.SupportedModels[models.M365Copilot]),
			provider.WithAPIKey(os.Getenv(m365auth.AccessTokenEnv)),
			provider.WithSystemMessage("Answer in one short sentence."),
		)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		resp, err := p.SendMessages(ctx, []message.Message{{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: "Reply with the words: Copilot is ready."}},
		}}, []tools.BaseTool{})
		if err != nil {
			return err
		}
		fmt.Printf("Copilot replied: %s\n", strings.TrimSpace(resp.Content))
		return nil
	},
}

// loadConfigQuietly loads the opencode config so the m365copilot settings apply.
// Errors about unrelated settings shouldn't block signing in.
func loadConfigQuietly(cmd *cobra.Command) *config.Config {
	cwd, _ := cmd.Flags().GetString("cwd")
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	cfg, err := config.Load(cwd, false)
	if err != nil && cfg == nil {
		fmt.Fprintf(os.Stderr, "warning: failed to load config: %v\n", err)
	}
	return cfg
}

func m365Settings(cmd *cobra.Command) m365auth.Settings {
	var settings m365auth.Settings
	if cfg := loadConfigQuietly(cmd); cfg != nil {
		settings = cfg.M365Copilot.AuthSettings()
	}
	if v, _ := cmd.Flags().GetString("tenant"); v != "" {
		settings.TenantID = v
	}
	if v, _ := cmd.Flags().GetString("client-id"); v != "" {
		settings.ClientID = v
	}
	return settings.WithDefaults()
}

// missingScopes lists the Copilot scopes the granted scope string lacks.
func missingScopes(granted string) []string {
	if granted == "" {
		return nil
	}
	have := map[string]bool{}
	for _, s := range strings.Fields(granted) {
		// Granted scopes may come back as full URIs, e.g. https://graph.microsoft.com/Mail.Read.
		if i := strings.LastIndexByte(s, '/'); i >= 0 {
			s = s[i+1:]
		}
		have[strings.ToLower(s)] = true
	}
	var missing []string
	for _, s := range m365auth.CopilotScopes {
		if !have[strings.ToLower(s)] {
			missing = append(missing, s)
		}
	}
	return missing
}

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func init() {
	m365LoginCmd.Flags().Bool("device-code", false, "Sign in with a code on another device instead of opening a browser")
	m365LoginCmd.Flags().String("tenant", "", "Entra ID tenant ID or domain (default: m365copilot.tenantId or \"organizations\")")
	m365LoginCmd.Flags().String("client-id", "", "Application (client) ID to sign in with (default: m365copilot.clientId)")
	m365StatusCmd.Flags().Bool("test", false, "Send a short test prompt to Microsoft 365 Copilot")
	for _, c := range []*cobra.Command{m365LoginCmd, m365LogoutCmd, m365StatusCmd} {
		c.Flags().StringP("cwd", "c", "", "Directory whose .opencode.json to use")
		// Errors here are about signing in, not about how the command was used.
		c.SilenceUsage = true
		m365Cmd.AddCommand(c)
	}
	rootCmd.AddCommand(m365Cmd)
}
