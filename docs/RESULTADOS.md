# Resultados da avaliação

Base: uma planta com 3 dispositivos, leituras diárias de 2026-01-01 a 2026-10-23. Clima observado: INMET (estação A701) até 2026-09-30 e Open-Meteo depois disso. Só entram leituras medidas com `quality = ok`.

A estação fica em São Paulo, mas a localização exata da planta não foi confirmada. Uma estação mais próxima pode mudar estes números.

## Geração esperada com o clima já conhecido

Validação temporal com janela expansível: mínimo de 60 dias de treino, novo ajuste a cada 7 dias, 514 pontos no total. É esta geração esperada que sustenta os alertas. **Não é uma previsão feita com antecedência.**

| Modelo | MAE (kWh) | RMSE (kWh) | R² | WAPE |
|---|---|---|---|---|
| Ridge (ativo) | 4,45 | 6,10 | 0,85 | 13,7% |
| XGBoost | 4,52 | 6,60 | 0,82 | 14,0% |
| Calibrado (`k × I`) | 4,88 | 6,85 | 0,81 | 15,1% |
| Físico | 5,78 | 7,52 | 0,77 | 17,8% |
| Repetir o dia anterior | 11,89 | 16,57 | -0,14 | 36,7% |

Um cálculo independente, escrito fora do código do projeto, reproduziu o modelo calibrado com R² de 0,804 e MAE de 4,91 kWh.

Na divisão cronológica 80/20 (141 pontos, um único período), o melhor foi o físico: R² de 0,81, contra 0,76 do ridge e 0,70 do XGBoost. A escolha do modelo muda conforme o período avaliado, e o ridge tem viés positivo nesse trecho. Com poucos meses de dados, a diferença entre esses modelos não é conclusiva.

## Efeito da correção da data

O carimbo da planilha indica o dia seguinte ao da geração. XGBoost com os mesmos dados e parâmetros, mudando apenas o alinhamento:

| Alinhamento | Validação | MAE (kWh) | R² |
|---|---|---|---|
| Corrigido | Janela expansível | 4,52 | 0,82 |
| Antigo | Janela expansível | 11,35 | 0,15 |
| Corrigido | 80/20 | 6,22 | 0,70 |
| Antigo | 80/20 | 12,86 | 0,00 |

Com o alinhamento antigo, o resultado fica próximo de repetir o dia anterior.

## Previsão feita com antecedência

Modelo calibrado, usando a radiação prevista pelo Open-Meteo antes de cada dia (arquivo Previous Runs), nos mesmos 517 dias de dispositivo. Esta é a medida certa para falar em previsão.

| Radiação usada | MAE (kWh) | RMSE (kWh) | R² |
|---|---|---|---|
| Observada no dia | 4,91 | 6,87 | 0,80 |
| Prevista 1 dia antes | 7,62 | 9,94 | 0,59 |
| Prevista 2 dias antes | 8,23 | 11,01 | 0,50 |
| Prevista 3 dias antes | 8,52 | 11,17 | 0,48 |
| Repetir o dia anterior (sem clima) | 11,82 | 16,44 | -0,12 |

Para reproduzir: `solarguard backtest --plant-id ID` e `ml/compare_xgboost.py`. A tabela de previsão com antecedência foi calculada em uma análise separada, que ainda não faz parte do binário.

## Detecção de falhas no histórico

Reprocessamento de toda a base: 23 incidentes.

| Regra | Incidentes |
|---|---|
| `zero_generation` | 6 |
| `under_expected` | 6 |
| `implausible_reading` | 5 |
| `missing_data` | 4 |
| `stuck_value` | 1 |
| `plant_no_generation` | 1 |
| `gradual_loss` | 0 |
| `peer_divergence` | 0 |

- **Leitura travada na TOA:** de 2026-02-16 a 2026-06-21 (121 dias, com datas já corrigidas), detectada em 2026-02-18, no terceiro dia.
- **Geração zerada na TOA:** de 2026-06-22 a 2026-07-31, sem interrupção.
- **`under_expected`:** os alertas costumam surgir em TOB e TOC nos mesmos dias, o que sugere diferença entre o clima da estação e o da planta, e não falha de equipamento. Um alerta é um indício para análise, não um diagnóstico.

## Plantas reais da Growatt

Cinco plantas com um inversor cada, identificadas por letras para não expor os clientes, com dados até 2026-11-03. Clima do Open-Meteo nas coordenadas de cada planta. Validação temporal com janela expansível, só em leituras válidas.

| Planta | Tipo | Pontos | Modelo ativo | MAE (kWh) | R² | MAE de repetir o dia anterior |
|---|---|---|---|---|---|---|
| A | MIN | 34 | Ridge | 0,84 | 0,11 | 0,87 |
| B | MAX | 93 | Calibrado | 21,66 | 0,51 | 40,12 |
| C | MAX | 226 | Calibrado | 30,05 | 0,47 | 38,85 |
| D | MAX | 178 | Ridge | 24,00 | 0,33 | 35,06 |
| E | MIN | 34 | Nenhum | 4,95 (ridge) | -0,09 | 4,61 |

O desempenho é bem menor que na planilha (R² de 0,85). Causas prováveis, ainda não testadas: clima de grade em vez de estação local, potência do inversor limitando os dias de sol e pouca variação diária no período avaliado. Na planta E, nenhum modelo superou a referência, e o sistema não ativou nenhum.

Os inversores MIN só têm os últimos 95 dias de histórico na API, por isso têm tão poucos pontos.

A divisão 80/20 avalia só o trecho final, e nela o resultado é mais fraco. O melhor entre os modelos climáticos foi sempre o ridge:

| Planta | Pontos | MAE ridge (kWh) | R² ridge | MAE de repetir o dia anterior |
|---|---|---|---|---|
| A | 19 | 1,11 | -1,79 | 0,88 |
| B | 31 | 16,59 | 0,17 | 30,72 |
| C | 58 | 30,07 | -2,25 | 11,55 |
| D | 48 | 24,50 | -0,39 | 20,13 |
| E | 19 | 3,15 | -7,72 | 1,59 |

Nesse trecho, repetir o dia anterior teve MAE menor que o ridge em 4 das 5 plantas. Portanto, nas plantas reais, o modelo ainda não demonstrou ganho estável. Com tão poucos meses, um resultado de um único período é frágil, mas não dá para afirmar que o modelo generaliza bem para elas. Nas regras de alerta, o valor esperado dessas plantas deve ser tratado com cautela. As regras que mais valem nelas são as que não dependem do modelo: travamento, geração zerada e falta de comunicação.

### Falhas encontradas nos dados reais

- **Planta B:** leitura travada por 23 dias, de 10/05 a 01/06, depois geração zerada desde 26/06 e sem comunicação desde 25/06.
- **Planta D:** geração zerada desde 27/08 e sem comunicação desde 07/09.
- **Planta C:** duas paradas, de 5 e 15 dias, com retorno posterior.
- **Planta E:** sem comunicação desde 03/11.

Na planilha e na planta B, o travamento da leitura veio antes da parada total. Dois casos não bastam para afirmar que uma coisa antecipa a outra, mas vale investigar com mais plantas.

## Limites

- Há no máximo cerca de 10 meses de dados por planta. Falta um ciclo anual completo.
- Nas plantas Growatt, cada planta tem um único inversor, então as regras que comparam dispositivos não se aplicam a elas.
- `gradual_loss` compara cada dispositivo com os demais, portanto não detecta uma perda igual em todos, como sujeira uniforme.
