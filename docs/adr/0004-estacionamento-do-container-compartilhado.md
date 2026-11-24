# 0004 — Estacionar o contêiner compartilhado durante a ociosidade

## Contexto

Remover o contêiner compartilhado durante a ociosidade apaga sua camada gravável. O checkpoint do workspace cobre o checkout e deixa de fora o estado reservado que vive nessa camada.

## Decisão

Parar o contêiner na ociosidade e reabrir a mesma instância após validar o vínculo da run com o workspace. Manter a remoção na limpeza explícita e no encerramento total do runner.

## Por quê

Remover e recriar o contêiner perde estado necessário à retomada. Estacionar a mesma instância conserva esse estado e mantém os testes reservados fora do checkout de implementação.

## Consequências

Parar o contêiner encerra seus processos e mantém a ocupação de disco da camada gravável. Se a instância for removida ou a máquina precisar ser substituída, o material reservado exige reprovisionamento a partir da origem aprovada.
