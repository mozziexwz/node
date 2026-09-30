FROM python:3.12-slim-bookworm
ENV MSBOOST_UNINSTALL_FIXTURE=1
COPY deploy/ /src/deploy/
COPY install.sh agent.sh /src/
CMD ["bash", "-c", "bash /src/deploy/uninstall_agent_test.sh && python3 /src/deploy/cleanup_agent_integration_test.py"]
