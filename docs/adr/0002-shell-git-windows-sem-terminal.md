# 0002 — Executar o shell do Git sem terminal no Windows

## Contexto

Quando `TERM` está ausente, o shell interno do Git for Windows pode resolver o Node por um alias de `winpty`. Nós nativos são executados sem TTY e dependem da ordem do `PATH` para encontrar o linker configurado.

## Decisão

Definir um `TERM` simples quando a variável estiver ausente na execução nativa pelo shell do Git. Preservar a precedência do `PATH` nativo.

## Por quê

A configuração de terminal evita a resolução por alias interativo. Priorizar as ferramentas distribuídas com o Git poderia trocar o linker escolhido pelo executor.

## Consequências

Scripts nativos sem terminal usam as ferramentas configuradas no `PATH`. Um `TERM` fornecido pelo ambiente é respeitado e a execução Linux segue pelo contrato existente.
