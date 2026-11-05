# Plano e regras de arquitetura

## Objetivo

Binário Go único que coleta geração de várias plantas, detecta falhas, estima a geração esperada, mostra tudo em um painel e expõe os dados a IAs via MCP.

## Regras que não podem ser quebradas

1. **Várias plantas sempre.** Nada pode assumir uma planta, três torres ou nomes como TOA. Toda consulta, alerta, previsão e tela é por planta ou dispositivo.
2. **A medição é imutável.** `readings.value` nunca é alterado depois da ingestão. A qualidade fica em `quality` e `quality_reason`. Valores estimados ficam em `imputations`.
3. **Período explícito.** Toda leitura guarda `period_start` e `period_end` em UTC. O dia operacional é calculado no fuso da planta.
4. **Modelos são avaliados só em dado medido** com `quality = ok`, sempre em validação temporal e comparados com a repetição do dia anterior.
5. **Alerta é indício, não diagnóstico.** Todo alerta guarda a evidência que o gerou.
6. **O LLM não decide nada.** Regras e modelos numéricos geram alertas e previsões. O LLM lê via MCP e só pode salvar rascunho de relatório.
7. **Conectores só leem.** Nunca implementar endpoints que criem, alterem ou apaguem dados no provedor.
8. **Sem segredos no repositório.** Os tokens das contas ficam criptografados no banco, e a chave fica no ambiente. `testdata` usa dados fictícios: nada de planilha real, identificadores reais ou payload capturado.

## Módulos

| Pacote | Responsabilidade |
|---|---|
| `internal/domain` | Tipos comuns e dia operacional |
| `internal/connector` | Contrato dos conectores e subpacotes por provedor |
| `internal/store` | SQLite e migrações |
| `internal/fileimport` | Importação de planilhas |
| `internal/ingest` | Fila persistente e limite de chamadas |
| `internal/quality` | Classificação das leituras |
| `internal/alerts` | Regras e ciclo de vida dos alertas |
| `internal/forecast` | Modelos em Go e backtest |
| `internal/reports` | Relatórios determinísticos |
| `internal/mcpserver` | Servidor MCP |
| `internal/web` | API e painel embutido |
| `ml/` | Comparação offline com XGBoost, fora do binário |

## Etapas

0. Contratos, schema e armazenamento.
1. Importador e qualidade de dados.
2. Conector Growatt e fila.
3. Conector de clima.
4. Motor de alertas e reprocessamento do histórico.
5. Backtest e previsão.
6. API e painel.
7. Servidor MCP e relatórios.
8. Teste ponta a ponta e documentação.

## Convenções

 - Código em inglês; documentação e textos da interface em português do Brasil.
- Testes escritos antes da implementação, com `go test -race ./...` verde.
- Novos subcomandos ficam em `cmd/solarguard/cmd_<nome>.go`, registrados por `register` em `init`.
- Nova migração: próximo número livre em `internal/store/migrations`. Migrações já commitadas não mudam.
