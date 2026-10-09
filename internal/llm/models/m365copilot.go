package models

const (
	ProviderM365Copilot ModelProvider = "m365copilot"

	// M365Copilot is Microsoft 365 Copilot, reached through the Copilot Chat API
	// in Microsoft Graph. The API doesn't expose model selection, so there is a
	// single model and no per-token cost (it's covered by the Copilot license).
	M365Copilot ModelID = "m365copilot.chat"
)

var M365CopilotModels = map[ModelID]Model{
	M365Copilot: {
		ID:                  M365Copilot,
		Name:                "Microsoft 365 Copilot",
		Provider:            ProviderM365Copilot,
		APIModel:            "microsoft-365-copilot",
		CostPer1MIn:         0,
		CostPer1MInCached:   0,
		CostPer1MOutCached:  0,
		CostPer1MOut:        0,
		ContextWindow:       128_000,
		DefaultMaxTokens:    8192,
		CanReason:           false,
		SupportsAttachments: false,
	},
}
