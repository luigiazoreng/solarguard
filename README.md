# SolarGuard

Monitoramento de geração fotovoltaica para várias plantas. O SolarGuard coleta as leituras, separa o que é medição válida do que é falha, calcula a geração esperada a partir do clima, gera alertas com evidência e entrega painel, relatórios e acesso para IA via MCP.

Projeto Integrador IV da UNIVESP.

## Como funciona

```text
Growatt, planilha ──> leituras imutáveis ──> classificação de qualidade
                                                   │
INMET, Open-Meteo ──> clima por dia ──> modelos ───┤
                                                   v
                                   alertas com evidência
                                                   │
                         ┌─────────────────────────┼──────────────┐
                         v                         v              v
                      painel web              relatórios      servidor MCP
```

- **A análise não depende de LLM.** Alertas e geração esperada vêm de regras, física e regressão, e podem ser auditados e reproduzidos. Um LLM conectado por MCP só lê os dados e escreve texto, e os rascunhos dele ficam marcados como rascunho.
- **Medido e estimado nunca se misturam.** Leituras não podem ser alteradas (o banco impede com um gatilho SQL) e os modelos só são avaliados em leituras válidas.
- **Um alerta é um indício para análise, não um diagnóstico.**
- **O conector da Growatt só lê.** Nenhum endpoint de escrita ou comando é chamado.

## Resultados

| Avaliação | Base | Resultado |
|---|---|---|
| Detecção de leitura travada | Planilha, 121 dias travados | Detectada no 3º dia |
| Geração esperada, com o clima do dia | Planilha, 514 pontos | R² 0,85, MAE 4,45 kWh |
| Previsão feita 1 dia antes | Planilha, 517 pontos | R² 0,59, MAE 7,62 kWh |
| Repetir o dia anterior (referência) | Planilha, 517 pontos | R² -0,12, MAE 11,82 kWh |
| Geração esperada nas plantas reais | 5 plantas Growatt | R² de -0,09 a 0,51 |

O modelo ainda não demonstrou ganho estável nas plantas reais: na divisão 80/20, repetir o dia anterior superou o ridge em 4 das 5. O ponto forte do sistema nelas é detectar travamento, geração zerada e falta de comunicação, que não dependem do modelo. Tabelas, método e limites estão em [docs/RESULTADOS.md](docs/RESULTADOS.md).

## Instalação

Para subir em uma VPS com Dokploy, siga [docs/DEPLOY.md](docs/DEPLOY.md). Para rodar localmente, é preciso ter Go 1.26 ou superior. Não há dependência de CGO nem de serviço externo de banco: os dados ficam em um arquivo SQLite, e o painel é embutido no binário.

```bash
git clone https://github.com/luigiazoreng/solarguard.git
cd solarguard
make build
cp .env.example .env
chmod 600 .env
```

Gere a chave com `openssl rand -base64 32`, preencha o `.env` e carregue as variáveis no terminal, porque o binário lê apenas o ambiente:

```bash
set -a; . ./.env; set +a
```

| Variável | Para que serve |
|---|---|
| `SOLARGUARD_SECRET_KEY` | Obrigatória. Criptografa os tokens salvos pelas contas. Se mudar, cada conta informa o token de novo. |
| `SOLARGUARD_DB` | Caminho do banco. Padrão: `./data/solarguard.db`. |
| `SOLARGUARD_ADDR` | Endereço do painel. Padrão: `127.0.0.1:8080`. |
| `SOLARGUARD_MCP_TOKEN` | Liga o MCP em `/mcp`, com acesso a todas as contas. Sem ela, a rota responde 404. |
| `SOLARGUARD_ALLOW_SIGNUP` | `true` permite novos cadastros, `false` aceita só as contas que já existem. Padrão: `true`. |
| `SOLARGUARD_TRUST_PROXY` | Use `true` só quando a porta for alcançável apenas pelo proxy reverso. Faz o bloqueio de senha valer por cliente. |
| `SOLARGUARD_GROWATT_BASE_URL` | Endereço da API da Growatt para novos tokens. Padrão: `https://openapi.growatt.com`. |

## Contas e Growatt

```bash
./bin/solarguard serve
```

Abra `http://127.0.0.1:8080`, crie uma conta e informe o token da Growatt em **Conta**. O token precisa ter permissão de API. Ele é conferido na Growatt, guardado criptografado no banco e nunca exibido de novo. A coleta começa sem reiniciar nada, e as primeiras leituras chegam em alguns minutos.

- Cada conta vê só as próprias usinas, e tentar abrir um dado de outra conta responde 404.
- Uma usina que já está no banco sem dono passa para a conta que salvar um token que a lista. Quem não tem esse token não a vê.
- As usinas de um token não podem estar em duas contas, para não haver dois coletores nelas.
- Remover o token para a coleta e mantém os dados.
- Não há recuperação de senha por e-mail. Para trocar a senha de uma conta, use `solarguard user-reset-password --email pessoa@exemplo.com`.

O `serve` faz tudo em um processo só:

- a cada 15 minutos, agenda a coleta dos dias já encerrados de cada conta com token, recuperando até 7 dias de atraso;
- uma vez por dia, depois das 02:00 no fuso de cada planta, roda clima, qualidade, modelos e alertas;
- roda essa sequência de novo quando chegam leituras depois da execução do dia.

Para buscar o histórico de uma planta, use o nome da fonte mostrado em **Gestão** e o ID que aparece no endereço da página da planta:

```bash
./bin/solarguard backfill --source NOME_DA_FONTE --plant-id 1 --from 2026-01-01 --to 2026-10-31
```

O comando só enfileira as tarefas, e o `serve` em execução as processa sem reiniciar. A API limita as chamadas por endpoint, então a coleta do histórico é lenta de propósito e continua de onde parou. Acompanhe com `solarguard queue --source NOME_DA_FONTE`. Os limites observados estão em [docs/GROWATT_API.md](docs/GROWATT_API.md).

Sem histórico, os alertas de falha já funcionam, mas não há geração esperada: cada modelo precisa de pelo menos 60 dias de leituras válidas.

Para coletar sem o painel, com o token em uma variável de ambiente:

```bash
./bin/solarguard source-add --name principal --token-env GROWATT_TOKEN
./bin/solarguard discover --source principal
./bin/solarguard collect --source principal
```

O `collect` fica em execução até receber Ctrl+C. Não o rode junto com o `serve` na mesma fonte. Uma fonte criada assim não tem dono: para vê-la no painel, use `source-assign`.

## O painel

| Página | O que faz |
|---|---|
| **Usinas** | Resumo de cada usina. Na página da usina, geração medida contra a esperada, leituras com problema e dispositivos. |
| **Alertas** | Lista com filtros e, em cada alerta, a evidência que o gerou. Permite reconhecer e resolver, com uma nota. |
| **Relatórios** | Gera relatórios semanais, mensais ou de um período e permite finalizá-los. O gráfico de previsão usa os dados guardados no próprio relatório, que não muda depois de gerado. Rascunhos escritos por IA ficam marcados. |
| **Gestão** | Nome, fuso e coordenadas das usinas, dados dos dispositivos, regras de alerta, fila de coleta e execução manual do processamento. |
| **Conta** | Token da Growatt e situação da sincronização. |

As regras de alerta são configuradas por usina em **Gestão**:

- **Sem depender do modelo:** leitura travada, geração zerada, leitura implausível, usina sem geração, dados ausentes, sem dados recentes e sem comunicação.
- **Precisa de geração esperada:** abaixo do esperado.
- **Precisa de dois ou mais dispositivos na usina:** divergência entre dispositivos e perda gradual.

## Uso com uma planilha

```bash
./bin/solarguard migrate
./bin/solarguard import-xlsx --file planilha.xlsx --source planilha \
  --plant-id exemplo --plant-name "Planta exemplo" --area-m2 42 --efficiency 0.18
./bin/solarguard plant-set-location --plant-id 1 --lat -23.4969 --lon -46.6199
./bin/solarguard pipeline --plant-id 1
./bin/solarguard report --plant-id 1 --kind monthly --from 2026-09-01 --to 2026-09-30
./bin/solarguard serve
```

Com o `serve` em execução, crie uma conta no painel e, em outro terminal, entregue a planilha a ela, porque uma fonte sem dono não aparece para ninguém:

```bash
./bin/solarguard source-assign --source planilha --email pessoa@exemplo.com
```

As armadilhas do formato (unidades misturadas e carimbo com um dia de atraso) estão em [docs/DADOS.md](docs/DADOS.md). A planilha do projeto não está no repositório, porque contém dados de um cliente.

Para usar uma estação do INMET em vez do clima de grade:

```bash
./bin/solarguard weather-import-inmet --file INMET.CSV --location "Mirante A701" \
  --timezone America/Sao_Paulo --plant-id 1
```

Execute `solarguard` sem argumentos para ver todos os comandos.

## Acesso por IA (MCP)

O servidor oferece 15 ferramentas de consulta e os prompts `relatorio_periodo` e `analise_alerta`. A única escrita possível é `save_report_draft`, que salva um rascunho de relatório. Campos de texto vindos de fontes externas são tratados como dados, nunca como instruções.

Na própria máquina, por stdio:

```json
{
  "mcpServers": {
    "solarguard": {
      "command": "/caminho/para/bin/solarguard",
      "args": ["mcp"],
      "env": { "SOLARGUARD_DB": "/caminho/para/data/solarguard.db" }
    }
  }
}
```

Pela rede, com o `serve` em execução e `SOLARGUARD_MCP_TOKEN` definido, use `http://127.0.0.1:8080/mcp` com o cabeçalho `Authorization: Bearer <token>`.

O MCP é para quem administra o servidor: tanto por stdio quanto pela rede, ele lê os dados de todas as contas.

## Segurança

- As senhas são guardadas com bcrypt, e os tokens com AES-256-GCM, usando uma chave que fica fora do banco.
- O painel exige login sempre. Para expor além da máquina, use HTTPS em um proxy reverso, como em [docs/DEPLOY.md](docs/DEPLOY.md).
- O painel usa proteção CSRF, CSP sem scripts inline e bloqueio após 5 senhas erradas.
- Nunca versione `.env`, `data/`, planilhas ou respostas brutas da API: eles já estão no `.gitignore`.

## Integrar outra fonte

Cada fonte é um conector que implementa a interface de [internal/connector](internal/connector): descobrir plantas, listar as tarefas de um dia e executar uma tarefa. A fila persistente e o respeito aos limites de chamadas ficam fora do conector. Use [internal/connector/growatt](internal/connector/growatt) como exemplo e registre o novo tipo em `ResolveSourceConnector`, em [internal/ingest/service.go](internal/ingest/service.go).

## Desenvolvimento

```bash
make check   # go vet e testes com detector de corrida
make build
```

O teste que chama o servidor de testes da Growatt só roda com `GROWATT_TEST_TOKEN` definido.

| Pasta | Conteúdo |
|---|---|
| `cmd/solarguard` | Comandos da linha de comando |
| `internal/connector` | Conectores de geração e clima |
| `internal/fileimport` | Importação de planilhas |
| `internal/ingest` | Fila de coleta, agendamento e início da coleta por conta |
| `internal/weather` | Sincronização do clima |
| `internal/quality` | Classificação das leituras |
| `internal/forecast` | Modelos, validação temporal e seleção do modelo ativo |
| `internal/alerts` | Regras de alerta |
| `internal/analytics` | Cálculos usados pelo painel, relatórios e MCP |
| `internal/reports` | Relatórios determinísticos |
| `internal/mcpserver` | Servidor MCP |
| `internal/pipeline` | Sequência diária por planta |
| `internal/web` | Painel |
| `internal/store` | SQLite e migrações |
| `internal/secrets` | Criptografia dos tokens |
| `ml` | Comparação offline com XGBoost, fora do binário |

As regras de arquitetura estão em [docs/PLANO.md](docs/PLANO.md).
