"""How an agent card is written. See score.py for how each way is scored."""
import re


def card_texts(name, card):
    """The ways a card can be written. See score.py for how each is scored."""
    short = f"{name}: {card['short']}"
    use = short + (" Use when: " + "; ".join(card["use"]) + "." if card["use"] else "")
    both = use + (" Not for: " + "; ".join(card["not"]) + "." if card["not"] else "")
    # A "not for" line often names the agent to use instead. That name does not
    # belong in the negative vector.
    negatives = [re.sub(r"\s*\([^)]*\)", "", n).strip() for n in card["not"]]
    return {"name": [name], "short": [short], "use": [use], "use_not": [both],
            "lines": [short] + [f"{name}: {u}" for u in card["use"]], "neg": [n for n in negatives if n]}
