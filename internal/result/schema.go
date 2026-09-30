package result

// Schema is the pretty-printed JSON Schema of result.json handed to the
// preparation prompt. It documents the contract enforced by Parse; runtime
// validation does not interpret it.
const Schema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "tower preparation result",
  "type": "object",
  "required": ["version", "status", "summary", "assumptions", "gate", "proposed_actions"],
  "properties": {
    "version": {
      "description": "Result format version.",
      "type": "integer",
      "const": 1
    },
    "status": {
      "description": "ready: stopped at the skill's first gate with a report. blocked: cannot continue without an answer to gate.question.",
      "type": "string",
      "enum": ["ready", "blocked"]
    },
    "summary": {
      "description": "1-3 sentences summarizing the findings. Must not be blank.",
      "type": "string",
      "minLength": 1
    },
    "confidence": {
      "description": "The skill's own confidence. Required when status is ready.",
      "type": "string",
      "enum": ["high", "medium", "low"]
    },
    "root_cause": {
      "description": "The identified root cause, if any.",
      "type": "string"
    },
    "assumptions": {
      "description": "Assumptions made during the investigation; may be empty.",
      "type": "array",
      "items": {"type": "string"}
    },
    "gate": {
      "description": "The stop point of this run.",
      "type": "object",
      "required": ["question"],
      "properties": {
        "question": {
          "description": "The exact question the skill would ask the user at the stop point. Must not be blank.",
          "type": "string",
          "minLength": 1
        },
        "options": {
          "description": "Suggested answers to the question.",
          "type": "array",
          "items": {"type": "string"}
        }
      }
    },
    "proposed_actions": {
      "description": "Proposed write actions; may be empty. ids are exactly 1..N in list order.",
      "type": "array",
      "items": {
        "type": "object",
        "required": ["id", "type", "title", "description"],
        "properties": {
          "id": {
            "description": "Position in the list, starting at 1.",
            "type": "integer",
            "minimum": 1
          },
          "type": {
            "type": "string",
            "enum": ["create-pr", "rollback", "scale", "silence", "suggest-ticket", "other"]
          },
          "title": {
            "description": "Must not be blank.",
            "type": "string",
            "minLength": 1
          },
          "description": {
            "description": "Must not be blank.",
            "type": "string",
            "minLength": 1
          },
          "preview": {
            "description": "Markdown preview (diff, command or ticket text).",
            "type": "string"
          }
        }
      }
    }
  },
  "if": {"properties": {"status": {"const": "ready"}}},
  "then": {"required": ["confidence"]}
}`
