# Armadilhas da planilha histórica

A planilha `Placa Solar - Consumo Diário.xlsx` não vai para o repositório. Colunas: `Torre`, `Endpoint`, `UTCDateTime`, `ActualValue (KW)`. São 291 datas de 2026-01-01 a 2026-10-24, com 3 dispositivos por data (873 linhas), e 6 datas ausentes.

## Unidades misturadas

O rótulo `KW` está errado.

- **Célula de texto, como `"42.32"`:** já em kWh. Não dividir. A razão mediana contra os outros dispositivos do dia é 1,004.
- **Célula numérica com valor absoluto abaixo de 1000 (ex: 44 e 2):** já em kWh. Não dividir. Os pares nesses dias geram 41,32 e 3,01 kWh.
- **Célula numérica com valor absoluto igual ou superior a 1000:** Wh. Dividir por 1000 (a menor célula verdadeira em Wh é 2001).

## Falhas conhecidas (datas conforme o carimbo da planilha)

- Dispositivo TOA travado em 18,11 de 2026-02-17 a 2026-06-22 (121 leituras), depois zero de 2026-06-23 a 2026-08-01.
- Picos de cerca de 8 a 9 milhões: 2026-05-22 TOC, 2026-08-11 TOC e TOB, 2026-08-26 TOA, 2026-09-23 TOC.

## Defasagem de um dia (confirmada)

O carimbo `03:00 UTC` é meia-noite local e guarda o total do dia **anterior**. Correlação da geração com a radiação global medida pelo INMET na estação A701, de 2026-01-01 a 2026-09-30:

| Série | Mesmo dia | Dia anterior |
|---|---|---|
| TOB + TOC | 0,50 | 0,93 |
| TOA, só em períodos válidos | 0,48 | 0,95 |

O Open-Meteo, fonte independente, mostra o mesmo padrão: 0,52 e 0,85 em TOB + TOC. Por isso `previous_day` é o padrão do importador.

## Física de referência

`E = I × A × η`, com `A = 42 m²` por torre e `η = 18%`, menos 0,4% para cada grau acima de 25 °C. Esses valores são parâmetros da planta, não constantes do código.

Nos dados de TOB, o ajuste `kWh = k × I` com o dia corrigido dá `k = 8,33`, contra `42 × 0,18 = 7,56` da fórmula. A área ou a eficiência reais podem ser cerca de 10% maiores que as do projeto.
