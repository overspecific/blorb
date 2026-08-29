---
name: planning
description: Build a plan for future work to be executed in stages
---

Planning is essential for good work - it lets the user decide what a good flow will be, and it lets you organise your thoughts. Plans in this codebase map to pull requests, each is a coherent set of changes.

Plans are written into `.agents/plans/{yyyymmdd}-{plan_name}/PLAN.md` - a new directory is created for each plan.  Additional artifacts can be placed alongside the plan file if needed; if none are needed then leave this out.

## Rules

- Each stage in the plan is a single commit, to be done in sequence
- Each stage can assume the previous stage is complete and does not need to mention dependencies on previous stages
- A stage cannot depend on stages after it
- A stage states what to build, which files, what behavior, and what tests
- Each stage must leave the tree building and `qc` passing, and end with the verification command, for example: "Verify with `bin/qc`."
- Every stage prompt ends with the same two sentences:
  > Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.
- The Todo list is an ordered checklist of the commits
- After each stage, stop and let the user review and commit before starting the next stage
- For a new PLAN.md, use the template below

## Template

```markdown
# <Plan name>

<A short overview of the plan, a single paragraph>

## Todo

- [ ] Commit 1: <short name>
- [ ] Commit 2: <short name>
- [ ] Commit 3: <short name>

---

## Commit 1: <short name>

> <A complete prompt for one commit: what to build, which files, what behavior, and what tests. It is handed to the agent as-is, so it must not depend on other stages and must leave the tree building and `qc` passing.>
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 2: <short name>

<as above>

## Commit 3: <short name>

<as above>
```

