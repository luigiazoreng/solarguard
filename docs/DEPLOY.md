# Instalação com Dokploy

O [docker-compose.yml](../docker-compose.yml) sobe um único container com o painel, a coleta e o MCP. O banco é um arquivo SQLite no volume `solarguard-data`, então não há container de banco. Ele sobrevive a novos deploys.

## Subir

1. No Dokploy, crie um serviço do tipo **Compose** apontando para este repositório, com o branch `main` e o caminho `./docker-compose.yml`. Enquanto o repositório for privado, o Dokploy precisa ter acesso a ele pelo GitHub.
2. Na aba **Environment**, defina as variáveis da tabela abaixo. Só a chave é obrigatória.
3. Crie um registro `A` no DNS apontando o domínio para a VPS.
4. Na aba **Domains**, adicione o domínio com o serviço `solarguard`, a porta `8080` e o HTTPS ligado com Let's Encrypt.
5. Faça o deploy.
6. Abra o domínio, crie uma conta e, em **Conta**, informe o token da Growatt. A coleta começa sem reiniciar nada, e as primeiras leituras chegam em alguns minutos.

| Variável | Para que serve |
|---|---|
| `SOLARGUARD_SECRET_KEY` | Obrigatória. Criptografa os tokens das contas. Gere com `openssl rand -base64 32` e guarde uma cópia fora da VPS. Se ela se perder, os dados continuam, mas cada conta precisa informar o token de novo. |
| `SOLARGUARD_ALLOW_SIGNUP` | `true` permite novos cadastros, `false` aceita só as contas que já existem. Padrão: `true`. |
| `SOLARGUARD_MCP_TOKEN` | Liga o MCP em `/mcp`. Veja [Acesso por IA](#acesso-por-ia-mcp). |

O Dokploy só lê essas variáveis no deploy, então faça um novo deploy depois de mudar alguma. O token da Growatt não vai aqui: cada conta o informa no painel.

Não publique a porta com `ports`. O compose define `SOLARGUARD_TRUST_PROXY=true`, que faz o painel aceitar o endereço do cliente informado pelo proxy, então só o proxy pode alcançar a porta. Sem isso, alguém poderia fugir do bloqueio de senha.

O cadastro fica aberto a quem acessar o domínio. Cada conta vê só as próprias usinas, mas consome recursos da VPS. Para fechar, crie as contas necessárias e defina `SOLARGUARD_ALLOW_SIGNUP=false`. Quem já tem conta continua entrando. Não feche antes de criar a primeira conta, ou ninguém conseguirá entrar até a variável voltar para `true`.

Mantenha **um único coletor por token**: pare qualquer `serve` ou `collect` que use o mesmo token em outra máquina.

Uma instalação nova só coleta os últimos 7 dias. Sem histórico não há geração esperada, porque cada modelo precisa de pelo menos 60 dias de leituras válidas.

## Levar um banco existente

Faça isto logo depois do primeiro deploy, antes de criar contas. O procedimento **substitui** o banco do volume.

Na máquina de origem, com o coletor parado:

```bash
sqlite3 data/solarguard.db ".backup /tmp/solarguard-envio.db"
scp /tmp/solarguard-envio.db VPS:/tmp/
rm /tmp/solarguard-envio.db
```

Pare o serviço no Dokploy e, na VPS, descubra o nome do volume, que termina em `_solarguard-data`:

```bash
docker volume ls --filter name=solarguard-data
```

Confira o conteúdo do volume. Depois de uma parada limpa, deve aparecer só `solarguard.db`. Se aparecer um arquivo `-wal`, inicie e pare o serviço de novo antes de continuar:

```bash
docker run --rm -v NOME_DO_VOLUME:/data alpine:3.22 ls /data
```

Depois copie o banco e inicie o serviço de novo pelo Dokploy:

```bash
docker run --rm -v NOME_DO_VOLUME:/data -v /tmp/solarguard-envio.db:/in.db:ro alpine:3.22 sh -c \
  'cp /in.db /data/solarguard.db && chown 10001 /data/solarguard.db && chmod 600 /data/solarguard.db'
rm /tmp/solarguard-envio.db
```

As usinas do banco importado ficam sem dono, e nenhuma conta as vê até comprovar acesso a elas. Quando alguém salva em **Conta** um token que lista essas usinas, a conta recebe as usinas e todo o histórico.

Uma planilha importada não tem token. Entregue a fonte a uma conta pelo terminal do container:

```bash
solarguard source-assign --source NOME_DA_FONTE --email pessoa@exemplo.com
```

## Rodar comandos

Pelo terminal do container no Dokploy. Para buscar o histórico de uma planta, use o nome da fonte mostrado em **Gestão** e o ID que aparece no endereço da página da planta:

```bash
solarguard backfill --source NOME_DA_FONTE --plant-id 1 --from 2026-01-01 --to 2026-10-31
```

O comando só enfileira as tarefas. O serviço em execução as processa sem reiniciar, desde que a conta tenha um token salvo. A coleta é lenta de propósito por causa dos limites da API. Para acompanhar:

```bash
solarguard queue --source NOME_DA_FONTE
```

Não há recuperação de senha por e-mail. Para trocar a senha de uma conta, rode o comando abaixo e digite a nova senha. As sessões já abertas continuam válidas até reiniciar o serviço.

```bash
solarguard user-reset-password --email pessoa@exemplo.com
```

## Acesso por IA (MCP)

`SOLARGUARD_MCP_TOKEN` liga a rota `/mcp`, e sem ele a rota responde 404. Esse token é de quem administra o servidor: ele lê os dados de **todas** as contas. Não o entregue a quem só tem uma conta no painel.

## Backup

Faça backup com frequência e guarde as cópias fora da VPS. Alguns inversores da Growatt só mantêm cerca de 95 dias de histórico, então o banco pode ser o único registro dos dias mais antigos.

O Dokploy faz backup de volumes nomeados para um destino S3, em **Volume Backups**. Ligue a opção de desligar o container durante a cópia. Para restaurar, o Dokploy exige que o volume de destino não exista e que os containers estejam parados.

Sem S3, gere uma cópia e tire-a do container. Na VPS, descubra o nome do container, que contém `solarguard`:

```bash
docker ps --filter name=solarguard
docker exec NOME_DO_CONTAINER sqlite3 /data/solarguard.db ".backup /data/backup.db"
docker cp NOME_DO_CONTAINER:/data/backup.db ./solarguard-backup.db
```

Depois copie `solarguard-backup.db` para fora da VPS. Para restaurar essa cópia, use os passos de [Levar um banco existente](#levar-um-banco-existente).

Os tokens no backup estão criptografados. Guarde a `SOLARGUARD_SECRET_KEY` separada dele.

## Atualizar

Faça um novo deploy. As migrações do banco rodam ao iniciar. Faça um backup antes: uma migração aplicada não é desfeita ao voltar para a versão anterior. As sessões ficam em memória, então cada deploy pede login de novo.

## Problemas comuns

| O que aparece | O que fazer |
|---|---|
| O serviço não inicia | Veja o log. `SOLARGUARD_SECRET_KEY` precisa ter pelo menos 32 caracteres, e `SOLARGUARD_ALLOW_SIGNUP` precisa ser `true` ou `false`. |
| "Não foi possível validar o token na Growatt" | Confira o token e se ele tem permissão de API. Sem ela, a Growatt recusa todas as chamadas. |
| "A Growatt só aceita esta consulta a cada 5 minutos" | Aguarde e tente de novo. Nada foi salvo. |
| "As usinas deste token já estão vinculadas a outra conta" | Entre na conta que já tem essas usinas. |
| Status **Parada** em **Conta** | A chave mudou e o token salvo não abre mais. Informe o token de novo. |
| "Muitas tentativas" no login | O bloqueio dura 15 minutos e vale só para aquele cliente. |
| Nenhuma geração esperada | Cada modelo precisa de pelo menos 60 dias de leituras válidas. Busque o histórico com `backfill`. |
