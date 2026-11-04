package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/luigiazoreng/solarguard/internal/store"
)

// RegisterAllPrompts binds the relatorio_periodo and analise_alerta prompts.
func RegisterAllPrompts(s *mcp.Server, st *store.Store) {
	// 1. relatorio_periodo
	promptRelatorio := &mcp.Prompt{
		Name:        "relatorio_periodo",
		Description: "Guia para elaboração de relatório de geração e incidentes de uma usina em um período especificado.",
		Arguments: []*mcp.PromptArgument{
			{
				Name:        "plant_id",
				Description: "Identificador numérico da usina (plant_id)",
				Required:    true,
			},
			{
				Name:        "from",
				Description: "Data inicial do período (YYYY-MM-DD)",
				Required:    true,
			},
			{
				Name:        "to",
				Description: "Data final do período (YYYY-MM-DD)",
				Required:    true,
			},
		},
	}

	s.AddPrompt(promptRelatorio, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		plantID := req.Params.Arguments["plant_id"]
		from := req.Params.Arguments["from"]
		to := req.Params.Arguments["to"]

		instruction := fmt.Sprintf(`Você é o assistente técnico do SolarGuard. Siga estas diretrizes obrigatórias para analisar os dados e redigir o relatório da usina %s para o período de %s a %s:

1. **Coleta de dados via ferramentas MCP**:
   - Utilize 'get_plant' para conhecer a usina e seus inversores/dispositivos.
   - Utilize 'get_generation' e 'get_expected_vs_actual' para avaliar a geração medida e a esperada nos dias válidos.
   - Utilize 'get_lost_energy' para obter a energia esperada em dias sem medição válida. Ela pode ter sido gerada e não registrada: nunca a descreva como perda confirmada.
   - Utilize 'get_data_quality' para verificar a integridade das leituras por dispositivo.
   - Utilize 'compare_devices' para a comparação justa entre dispositivos da usina nos dias simultaneamente válidos.
   - Utilize 'list_alerts' para levantar os incidentes ocorridos no período.
   - Utilize 'get_model_metrics' para documentar o modelo numérico ativo.
   - Se aplicável, utilize 'get_forecast' para verificar a previsão futura.

2. **Rigor e distinção de números**:
   - Cite números exatos obtidos das ferramentas.
   - Distinga com clareza o que é **geração medida** (leituras reais com qualidade ok), o que é **geração esperada** (calculada pelo modelo numérico a partir do clima observado) e o que é **estimativa** (como energia perdida).
   - Nunca invente medições e nunca apresente zero como se fosse uma medição quando os dados estiverem ausentes.

3. **Interpretação de alertas**:
   - Lembre-se: **Um alerta é um indício para análise, nunca um diagnóstico definitivo**. Alertas indicam anomalias nas medições ou desvios em relação a modelos ou pares, e servem como evidência para o operador investigar.

4. **Salvamento do resultado**:
   - Concluída a análise, salve o relatório em formato Markdown chamando a ferramenta 'save_report_draft' (com plant_id=%s, period_start=%s, period_end=%s, title apropriado e cited_data preenchido).`,
			plantID, from, to, plantID, from, to)

		return &mcp.GetPromptResult{
			Description: fmt.Sprintf("Diretrizes para relatório da usina %s (%s a %s)", plantID, from, to),
			Messages: []*mcp.PromptMessage{
				{
					Role: "user",
					Content: &mcp.TextContent{
						Text: instruction,
					},
				},
			},
		}, nil
	})

	// 2. analise_alerta
	promptAlerta := &mcp.Prompt{
		Name:        "analise_alerta",
		Description: "Guia para análise técnica das evidências de um alerta detectado no SolarGuard.",
		Arguments: []*mcp.PromptArgument{
			{
				Name:        "alert_id",
				Description: "Identificador numérico do alerta a analisar",
				Required:    true,
			},
		},
	}

	s.AddPrompt(promptAlerta, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		alertID := req.Params.Arguments["alert_id"]

		instruction := fmt.Sprintf(`Você é o assistente técnico especialista do SolarGuard. Siga estas diretrizes para analisar tecnicamente o alerta %s:

1. **Obtenção e análise da evidência**:
   - Chame 'get_alert' para obter o alerta, sua regra, datas de vigência e as evidências numéricas registradas em 'evidence_json'.
   - Chame 'get_plant' para conhecer a usina e os dispositivos associados.
   - Chame 'compare_devices' e 'get_weather' para o período relevante ao alerta, a fim de contextualizar se o desvio afetou apenas um equipamento ou toda a planta.

2. **Raciocínio fundamentado em evidências**:
   - Um alerta é um indício para análise, não um diagnóstico. Trate os dados como evidências técnicas observadas e apresente hipóteses com base em dados numéricos citados explicitamente.
   - Verifique se a causa pode ser climática (ex: irradiação baixa na região), sujeira/sombreamento (divergência em relação aos pares) ou falha de comunicação/medição (leitura travada, dados ausentes).

3. **Registro do parecer**:
   - Elabore um parecer técnico em Markdown estruturado (Contexto, Evidências Observadas, Análise Comparativa e Recomendações).
   - Salve a análise como rascunho de relatório chamando a ferramenta 'save_report_draft'.`,
			alertID)

		return &mcp.GetPromptResult{
			Description: fmt.Sprintf("Diretrizes para análise técnica do alerta %s", alertID),
			Messages: []*mcp.PromptMessage{
				{
					Role: "user",
					Content: &mcp.TextContent{
						Text: instruction,
					},
				},
			},
		}, nil
	})
}
