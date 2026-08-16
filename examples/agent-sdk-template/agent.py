"""Reference agent using the agent-orc Python SDK.

This agent demonstrates the full agent-orc SDK surface:
  - OpenAI-compatible model-router integration (automatic)
  - Custom tool registration with @agent.tool
  - Built-in lifecycle tools: done, fail, ask, handoff, spawn
  - Checkpoint save/restore helpers
  - Egress configuration for durable result delivery

Deploy as an Agent with framework: openai-compatible.
The model-router injects OPENAI_BASE_URL=http://localhost:8080 automatically.
"""

from agentorc import Agent

agent = Agent(
    system_prompt=(
        "You are a helpful research assistant. Use the tools available to you "
        "to answer questions accurately. When you have a final answer, call "
        "_done with your output. If you need more information from the user, "
        "use _clarify. If the task can be broken into sub-tasks, use _spawn "
        "to delegate. If the task is better handled by another agent, use "
        "_handoff."
    ),
    temperature=0.7,
)


@agent.tool
def search_web(query: str) -> str:
    """Search the web for information about a topic.

    Uses the _rag_search built-in tool if a KnowledgeBase is configured,
    otherwise returns a placeholder.
    """
    # In a real agent, this would call an external search API.
    # The model-router injects _rag_search when knowledgeBases are configured.
    return f"Search results for: {query}"


@agent.tool
def calculate(expression: str) -> str:
    """Evaluate a mathematical expression safely."""
    try:
        # Note: in production, use a safe expression evaluator.
        result = eval(expression, {"__builtins__": {}}, {})
        return str(result)
    except Exception as e:
        return f"Error: {e}"


@agent.tool
def summarize(text: str, max_length: int = 100) -> str:
    """Summarize text to a maximum length."""
    if len(text) <= max_length:
        return text
    return text[:max_length] + "..."


def main():
    """Entry point for the agent process.

    The AGENTORC_INPUT environment variable contains the run input.
    """
    import os

    input_text = os.environ.get("AGENTORC_INPUT", "")
    if not input_text:
        # HTTP mode: input is POSTed to /invoke.
        # The agentorc.Agent.run() method handles the OpenAI-compatible
        # chat completion loop with the model-router.
        print("No input provided", flush=True)
        return

    # Run the agent until it reaches a terminal state.
    result = agent.run(input=input_text)

    print(f"Run completed: phase={result.phase}", flush=True)
    if result.phase == "Succeeded":
        print(f"Output: {result.output}", flush=True)
    elif result.phase == "Failed":
        print(f"Failed: {result.failure_reason}", flush=True)
    elif result.phase == "WaitingForInput":
        print(f"Waiting for input: {result.output}", flush=True)
    elif result.phase == "HandedOff":
        print("Task handed off to another agent", flush=True)


if __name__ == "__main__":
    main()
