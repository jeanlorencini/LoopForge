# 0003 — Vincular aprovação, testes e revisão à entrega do SOP

## Contexto

A espera por aprovação, uma interrupção ou uma edição posterior podem separar o material que passou por um gate daquele que a run usa na próxima etapa.

## Decisão

Validar especificação e pré-requisitos antes da implementação. Manter a aprovação humana prevista no SOP e conferir, ao congelar a entrada, os bytes que passaram pela validação. Reutilizar um selo válido com evidência dos testes na retomada. Vincular a revisão e o teste final à árvore Git a ser entregue.

## Por quê

O status de um gate, sozinho, só indica que a etapa passou em algum momento. A prova vinculada ao conteúdo permite reaproveitar uma validação anterior apenas enquanto ela ainda se aplica.

## Consequências

A perda do selo exige recuperação a partir da origem aprovada. Uma árvore Git que mudou depois da revisão exige nova verificação antes da entrega. A candidata foi validada localmente.
