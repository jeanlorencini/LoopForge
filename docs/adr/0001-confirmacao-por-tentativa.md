# 0001 — Identificar a tentativa na confirmação do runner

## Contexto

O formato legado omite a tentativa. Se a tarefa recebe outro lease, esse formato torna indistinguíveis os resultados antigo e atual.

## Decisão

Manter a confirmação pelo lease para clientes legados. O runner atualizado envia a tentativa. A API valida o número informado e aceita a repetição do mesmo resultado terminal sob a mesma autorização.

## Por quê

Exigir a tentativa de todos os clientes quebraria versões já instaladas. Usar só o lease para todos manteria a ambiguidade mesmo após atualizar o runner.

## Consequências

O runner atualizado pode repetir a confirmação após perder a resposta da API, sem executar novamente o comando. O formato legado mantém a limitação de distinguir tentativas.
