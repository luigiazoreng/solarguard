# API Growatt: o que foi verificado

Fontes: coleção pública no Postman e testes de leitura em `test.growatt.com` em 2026-11-04. O servidor de testes pode se comportar diferente da produção.

- **Bases:** produção `https://openapi.growatt.com`, testes `https://test.growatt.com`.
- **Autenticação:** cabeçalho `token: <token>`.
- **Envelope:** `{"data": ..., "error_code": 0, "error_msg": ""}`. Em erro, `data` pode ser string.
- **HTTP 200 não indica sucesso:** sempre checar `error_code`.

## Limites

- `10012 error_frequently_access`: uma chamada a cada 5 minutos.
- **Observado no servidor de testes:** repetir a mesma chamada falha na hora, mas outros endpoints e o mesmo endpoint com outro `plant_id` respondem. Por isso a chave do limite é endpoint + parâmetros. Falta confirmar em produção.
- `plant/energy` e `plant/list`: documentados com 10 chamadas por dia.
- `10013`: mais de 100 itens por página. `10014`: mais de 250 páginas.
- Terceiros observaram histórico de cerca de 95 dias em algumas chamadas. Não confirmado.

## Endpoints usados (somente leitura)

| Endpoint | Parâmetros | Resposta em `data` |
|---|---|---|
| `GET /v1/plant/list` | `page`, `perpage` | `plants[]`, `count` |
| `GET /v1/plant/details` | `plant_id` | `timezone`, `latitude`, `longitude`, `peak_power`, `installed_panel_area` |
| `GET /v1/plant/data` | `plant_id` | `today_energy`, `last_update_time`, `timezone` |
| `GET /v1/plant/energy` | `plant_id`, `start_date`, `end_date` (até 7 dias), `time_unit` | `energys[]` com `date` e `energy` em kWh, como texto |
| `GET /v1/plant/power` | `plant_id`, `date` | `powers[]` com `time` e `power` em W, `count` |
| `GET /v1/device/list` | `plant_id`, `page`, `perpage` | `devices[]`, `count` |
| `GET /v1/device/inverter/day_energy` | `device_sn`, `date` | no nível raiz: `device_sn`, `dataloggerSn`, `data` |

## Cuidados

- `plant_id` chega como inteiro. `peak_power` pode ser inteiro ou decimal. Vários números chegam como texto.
- `device/list` retorna `lost` (0 online, 1 desconectado) e `status` por tipo. No tipo 1, `3` é falha.
- A unidade de `day_energy.data` não está documentada. Presume-se kWh, a confirmar com dados reais.
- O servidor de testes não tem inversores cadastrados: `day_energy` só é testável com token de produção.
- Alarmes não estão na coleção. Um wrapper de terceiros usa `GET /v1/device/inverter/alarm`, não verificado.
