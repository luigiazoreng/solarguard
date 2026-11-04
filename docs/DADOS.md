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

## Provável defasagem de um dia

O carimbo `03:00 UTC` é meia-noite local, então deve ser o total do dia **anterior**. A correlação de TOB+TOC com a radiação do dia anterior é 0,91, contra 0,59 no mesmo dia. A radiação foi digitalizada de uma figura, então a hipótese deve ser confirmada com o arquivo real do INMET.

O importador trata isso como `stamp_semantics`, com padrão `previous_day`.

## Física de referência

`E = I × A × η`, com `A = 42 m²` por torre e `η = 18%`, menos 0,4% para cada grau acima de 25 °C. Esses valores são parâmetros da planta, não constantes do código.
