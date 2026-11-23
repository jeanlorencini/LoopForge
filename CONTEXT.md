<!-- autopilot:glossary -->
# Glossário

- **Run** — instância de execução de um workflow.
- **Tentativa.** Execução numerada de um nó da run, preservada na retomada após uma interrupção. Uma tentativa pode despachar mais de uma operação ao runner. A tarefa tem sua própria numeração de tentativas na fila. O runner atualizado usa esse número da fila ao confirmar o resultado.
- **Lease.** Autorização temporária de posse. O lease da run pertence ao worker do motor que a coordena. O lease da tarefa pertence ao runner que a executa e confirma seu resultado.
- **Gate** — etapa que condiciona o avanço da run a uma verificação ou decisão humana.
- **Selo** — registro do conteúdo congelado e da evidência de testes que permite conferir a entrada ao retomar o SOP.
- **Checkpoint do workspace** — cópia recuperável do workspace vinculado ao runner.
- **Sandbox compartilhado** — ambiente Docker reutilizado por nós compatíveis da mesma run.
