# Comparação Offline com XGBoost

Este diretório contém os scripts para avaliação comparativa offline dos dados do SolarGuard utilizando XGBoost, fora do binário principal em Go.

## Requisitos

- Python 3.10 ou superior
- Ambiente virtual dedicado em `ml/.venv`

## Configuração do Ambiente

Crie o ambiente virtual e instale as dependências:

```bash
python3 -m venv ml/.venv
ml/.venv/bin/pip install --upgrade pip
ml/.venv/bin/pip install -r ml/requirements.txt
```

## Execução

O script lê o banco SQLite diretamente em modo somente leitura (`mode=ro`), aplicando os mesmos filtros de qualidade (`quality = 'ok'`), a mesma preferência de provedor de clima (INMET prioritário, seguido por Open-Meteo) e as mesmas variáveis preditoras do modelo ridge.

Para executar o comparativo:

```bash
ml/.venv/bin/python ml/compare_xgboost.py --db ./data/solarguard.db --plant-id 1 --out ./data/results.json
```

### Parâmetros da Linha de Comando

- `--db PATH`: caminho para o banco SQLite do SolarGuard (aberto em modo somente leitura).
- `--plant-id ID`: identificador numérico da planta a ser avaliada.
- `--out PATH`: arquivo JSON de saída para as métricas agregadas (deve ficar sob `data/`, que é ignorado pelo Git).

## Modelos Avaliados

1. **XGBoost Walk-Forward**: janela expansível iniciando com no mínimo 60 dias de treino e reajuste a cada 7 dias, prevendo os dias subsequentes sem vazamento temporal.
2. **XGBoost 80/20 Cronológico**: divisão cronológica simples (primeiros 80% para treino, últimos 20% para teste), sem embaralhamento.
3. **xgboost_old_alignment**: repetição dos cenários acima deslocando a data do clima em +1 dia, simulando o alinhamento errôneo anterior para evidenciar o impacto da correção de data.
